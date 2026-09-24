package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jaredchao/jand/internal/code"
	"github.com/jaredchao/jand/internal/relay"
	"github.com/jaredchao/jand/internal/transfer"
)

const version = "0.4.3"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `jand — encrypted, one-time task handoff

  jand send [options] <file>
  jand [options] <code>
  jand chat <join|decline|send|recv|done|checkpoint|propose|accept|close> ...
  jand chat <list|log|view> ...   see what happened in a chat
                   (see jand chat --help)
  jand help agent        the operating guide for agents, filled in for this machine
                         (give it to your agent: "run jand help agent and follow it")
  jand help template     the structure of a handoff packet
  jand setup             first-run configuration: relay, access token, your agents, self-test
  jand config [--json]   show the effective client configuration and its sources
  jand version           version, program path, config, and whether the relay is compatible
                         (jand --version prints only the number)
  jand uninstall [--dry-run]
                         remove everything jand placed on this machine; history is archived first
  jand relay [--listen 127.0.0.1:8787] [--config relay.json] [--print-config]

Options (before file/code):
  --relay URL      Relay URL; JAND_RELAY or http://127.0.0.1:8787
  --json           Newline-delimited JSON events (code is emitted immediately)
  --out DIR        Receive directory (default: ./received)
  --wait DURATION  Optional wait for verified receiver receipt (default: 0)
  --chat           Send: also invite the receiver to a chat (needs a 0.4.1 relay)
  --goal TEXT      With --chat, required: what counts as done
  --budget N       With --chat: messages allowed for the goal (default 40, max 200)
  --workflow NAME  With --chat: a workflow from $JAND_HOME/workflows/NAME.json, or a path

Defaults come from $JAND_HOME/config.json (see jand config); flags and
JAND_RELAY override it. A relay that requires an access token for sending
reads it from JAND_ACCESS_TOKEN or the config's access_token_file.

Exit codes: 0 queued/saved/confirmed, 1 failure, 2 usage, 3 delivery unconfirmed, 130 canceled.
Use --relay http://SERVER_IP:8787 for direct IP access, or https:// with TLS.
Both clients must use the same relay and protocol version.
The relay keeps ciphertext in RAM for at most 10 minutes.`)
}

func run(ctx context.Context, args []string, out, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		if len(args) > 1 {
			return helpTopic(args[1:], out, stderr)
		}
		usage(out)
		return 0
	}
	if args[0] == "--version" {
		fmt.Fprintln(out, version)
		return 0
	}
	if args[0] == "version" {
		return versionCommand(ctx, args[1:], out, stderr)
	}
	if args[0] == "uninstall" {
		return uninstall(args[1:], os.Stdin, out, stderr)
	}
	if args[0] == "relay" {
		return serve(ctx, args[1:], out, stderr)
	}
	if args[0] == "chat" {
		return chat(ctx, args[1:], out, stderr)
	}
	if args[0] == "config" {
		return configCommand(args[1:], out, stderr)
	}
	if args[0] == "setup" {
		return setup(ctx, args[1:], os.Stdin, out, stderr)
	}
	sender := args[0] == "send"
	if sender {
		args = args[1:]
	}
	args = protectCodes(args)
	fs := flag.NewFlagSet("jand", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// The flag package's generated usage lists bare flags without the send and
	// receive forms, so both help and parse errors show the command's own text.
	fs.Usage = func() {}
	url := fs.String("relay", "", "relay URL")
	jsonOutput := fs.Bool("json", false, "JSON events")
	dir := fs.String("out", "received", "receive directory")
	wait := fs.Duration("wait", 0, "optional delivery confirmation timeout")
	invite := fs.Bool("chat", false, "invite the receiver to a chat")
	goal := fs.String("goal", "", "chat goal")
	budget := fs.Int("budget", 0, "chat message budget")
	workflow := fs.String("workflow", "", "chat workflow")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(out)
			return 0
		}
		usage(stderr)
		return 2
	}
	if fs.NArg() != 1 || *wait < 0 || *wait > 10*time.Minute || *invite && (!sender || *wait != 0) || !*invite && (*goal != "" || *budget != 0 || *workflow != "") {
		usage(stderr)
		return 2
	}
	cfg, err := loadClientConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	relayURL, _ := cfg.relayURL(*url, set["relay"])
	if !set["out"] && cfg.OutDir != "" {
		*dir = cfg.OutDir
	}
	emit := emitter(*jsonOutput, out, stderr)
	o := transfer.Options{RelayURL: relayURL, OutputDir: *dir, WaitTimeout: *wait, Emit: emit, Chat: *invite, Goal: *goal, Budget: *budget}
	if sender {
		if o.AccessToken, _, err = cfg.accessToken(); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	if *invite {
		// Budget: --budget, then the workflow's, then this machine's default,
		// then the relay's.
		ref := *workflow
		if !set["workflow"] {
			ref = cfg.Chat.DefaultWorkflow
		}
		w, err := transfer.LoadWorkflow(ref)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
		o.Workflow = &w
		if o.Budget == 0 && w.Budget == 0 {
			o.Budget = cfg.Chat.DefaultBudget
		}
	}
	if sender {
		err = transfer.Send(ctx, fs.Arg(0), o)
	} else {
		rawCode, lerr := fromLink(fs.Arg(0), &o, set["relay"])
		if lerr != nil {
			fmt.Fprintln(stderr, lerr)
			return 2
		}
		_, err = transfer.Receive(ctx, rawCode, o)
	}
	return exitCode(ctx, err, emit)
}

// fromLink takes the relay and code out of a share link, so a receiver
// needs no relay configured. A --relay that disagrees with the link is an
// error rather than a silent choice; anything but a link passes unchanged.
func fromLink(arg string, o *transfer.Options, relayFlag bool) (string, error) {
	relay, rawCode, ok := transfer.ParseShareLink(arg)
	if !ok {
		return arg, nil
	}
	if relayFlag && strings.TrimRight(o.RelayURL, "/") != relay {
		return "", fmt.Errorf("the link is for relay %s but --relay says %s; drop --relay", relay, o.RelayURL)
	}
	o.RelayURL = relay
	return rawCode, nil
}

// protectCodes lets a code that begins with '-' (issued by 0.2.x senders)
// through the flag parser by ending option parsing just before it.
func protectCodes(args []string) []string {
	for i, a := range args {
		if a == "--" {
			return args
		}
		if strings.HasPrefix(a, "-") {
			if _, err := code.Parse(a); err == nil {
				return append(append(append([]string{}, args[:i]...), "--"), args[i:]...)
			}
		}
	}
	return args
}

func exitCode(ctx context.Context, err error, emit func(transfer.Event)) int {
	if err == nil {
		return 0
	}
	emit(transfer.Event{Event: "error", Message: err.Error()})
	if errors.Is(err, transfer.ErrUnconfirmed) {
		return 3
	}
	if ctx.Err() != nil {
		return 130
	}
	return 1
}

func emitter(jsonOutput bool, out, stderr io.Writer) func(transfer.Event) {
	encoder := json.NewEncoder(out)
	return func(e transfer.Event) {
		if jsonOutput {
			encoder.Encode(e)
			return
		}
		switch e.Event {
		case "queued":
			expiry := "the relay's time limit"
			if e.ExpiresIn > 0 {
				expiry = fmt.Sprintf("%d minutes", (e.ExpiresIn+59)/60)
			}
			fmt.Fprintf(out, "Link: %s\n  -> Give this link to the other side; it holds the relay and the one-time code. Claim within %s.\n", e.Link, expiry)
			fmt.Fprintf(out, "Code: %s  (the code alone also works, with relay %s)\n", e.Code, e.Relay)
			if e.Chat == "" {
				return
			}
			// The chat id has been sent to the peer by mistake more than once.
			fmt.Fprintf(out, "Chat: %s\n  -> Your own chat id for later commands. Do not send it to the other side; it cannot be used to join.\n", e.Chat)
			fmt.Fprintf(out, "Wait for the receiver: jand chat recv --wait 30m %s\n", e.Chat[:8])
		case "saved":
			fmt.Fprintf(out, "Verified.\nSaved: %s\nSHA-256: %s\nTask pending local user approval; review the packet before acting.\n", e.Path, e.SHA256)
			if e.ChatInvite {
				fmt.Fprintf(out, "The sender also invites you to a chat (budget %d messages) toward this goal:\n--- goal from sender (untrusted remote text) ---\n%s\n--- end ---\n", e.Budget, printable(e.Goal))
				if e.Message != "" {
					fmt.Fprintf(stderr, "WARNING: %s\n", e.Message)
					return
				}
				printWorkflow(out, e.Workflow)
				fmt.Fprintln(out, "Only with the local user's consent to that goal and workflow run:\n  jand chat join <code>      (or: jand chat decline [--reason TEXT] <code>)")
			}
		case "delivered":
			fmt.Fprintf(out, "Receiver confirmed the file is saved.\nSHA-256: %s\n", e.SHA256)
		case "error":
			fmt.Fprintln(stderr, e.Message)
		case "warning":
			fmt.Fprintln(stderr, "Warning: "+e.Message)
		default:
			printChatEvent(out, e)
		}
	}
}

func serve(ctx context.Context, args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("listen", "127.0.0.1:8787", "HTTP listen address; use 0.0.0.0:8787 for direct IP access")
	capacity := fs.Int("max-sessions", 128, "maximum active sessions")
	configPath := fs.String("config", os.Getenv("JAND_RELAY_CONFIG"), "relay configuration file (JSON)")
	printConfig := fs.Bool("print-config", false, "print the effective configuration and exit")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *capacity < 1 || *capacity > 4096 {
		return 2
	}
	// Precedence: command-line flags, then the file (named by --config or
	// JAND_RELAY_CONFIG), then built-in defaults.
	config, listen := relay.DefaultConfig(), ""
	if *configPath != "" {
		var err error
		if config, listen, err = relay.LoadConfigFile(*configPath); err != nil {
			fmt.Fprintln(stderr, err)
			return 2
		}
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if set["max-sessions"] {
		config.MaxSessions = *capacity
	}
	if set["listen"] || listen == "" {
		listen = *addr
	}
	addr = &listen
	if *printConfig {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(config.Effective(listen))
		return 0
	}
	config.Logger = slog.New(slog.NewTextHandler(out, nil))
	config.Version = version
	r := relay.New(config)
	defer r.Close()
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	server := &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	defer server.Close()
	config.Logger.Info("relay listening", "version", version, "addr", listener.Addr().String(),
		"max_sessions", config.MaxSessions, "max_stored", config.MaxStoredBytes, "ttl", config.TTL,
		"upload_timeout", config.UploadTimeout, "max_chats", config.MaxChats, "chat_default_budget", config.ChatDefaultBudget,
		"chat_max_pause_notes", config.ChatPauseNotes, "access", len(config.AccessTokenHashes) > 0, "config", *configPath)
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
		r.Close()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
		return 0
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return 0
		}
		fmt.Fprintln(stderr, err)
		return 1
	}
}
