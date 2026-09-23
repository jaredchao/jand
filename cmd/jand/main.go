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
	"syscall"
	"time"

	"github.com/jaredchao/jand/internal/relay"
	"github.com/jaredchao/jand/internal/transfer"
)

const version = "0.2.0-dev"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `jand — encrypted, one-time task handoff

  jand send [options] <file>
  jand [options] <code>
  jand relay [--listen 127.0.0.1:8787]

Options (before file/code):
  --relay URL      Relay URL; JAND_RELAY or http://127.0.0.1:8787
  --json           Newline-delimited JSON events (code is emitted immediately)
  --out DIR        Receive directory (default: ./received)
  --wait DURATION  Optional wait for verified receiver receipt (default: 0)

Exit codes: 0 queued/saved/confirmed, 1 failure, 2 usage, 3 delivery unconfirmed, 130 canceled.
Use --relay http://SERVER_IP:8787 for direct IP access, or https:// with TLS.
Both clients must use the same relay and protocol version.
The relay keeps ciphertext in RAM for at most 10 minutes.`)
}

func run(ctx context.Context, args []string, out, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		usage(out)
		return 0
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Fprintln(out, version)
		return 0
	}
	if args[0] == "relay" {
		return serve(ctx, args[1:], out, stderr)
	}
	sender := args[0] == "send"
	if sender {
		args = args[1:]
	}
	fs := flag.NewFlagSet("jand", flag.ContinueOnError)
	fs.SetOutput(stderr)
	// The flag package's generated usage lists bare flags without the send and
	// receive forms, so both help and parse errors show the command's own text.
	fs.Usage = func() {}
	url := os.Getenv("JAND_RELAY")
	if url == "" {
		url = "http://127.0.0.1:8787"
	}
	fs.StringVar(&url, "relay", url, "relay URL")
	jsonOutput := fs.Bool("json", false, "JSON events")
	dir := fs.String("out", "received", "receive directory")
	wait := fs.Duration("wait", 0, "optional delivery confirmation timeout")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(out)
			return 0
		}
		usage(stderr)
		return 2
	}
	if fs.NArg() != 1 || *wait < 0 || *wait > 10*time.Minute {
		usage(stderr)
		return 2
	}
	encoder := json.NewEncoder(out)
	emit := func(e transfer.Event) {
		if *jsonOutput {
			encoder.Encode(e)
			return
		}
		switch e.Event {
		case "queued":
			fmt.Fprintf(out, "Code: %s\nEncrypted transfer queued in relay RAM (expires in 10 minutes).\n", e.Code)
		case "saved":
			fmt.Fprintf(out, "Verified.\nSaved: %s\nSHA-256: %s\nTask pending local user approval; review the packet before acting.\n", e.Path, e.SHA256)
		case "delivered":
			fmt.Fprintf(out, "Receiver confirmed the file is saved.\nSHA-256: %s\n", e.SHA256)
		case "error":
			fmt.Fprintln(stderr, e.Message)
		}
	}
	o := transfer.Options{RelayURL: url, OutputDir: *dir, WaitTimeout: *wait, Emit: emit}
	var err error
	if sender {
		err = transfer.Send(ctx, fs.Arg(0), o)
	} else {
		_, err = transfer.Receive(ctx, fs.Arg(0), o)
	}
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

func serve(ctx context.Context, args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("relay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("listen", "127.0.0.1:8787", "HTTP listen address; use 0.0.0.0:8787 for direct IP access")
	capacity := fs.Int("max-sessions", 128, "maximum active sessions")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *capacity < 1 || *capacity > 4096 {
		return 2
	}
	config := relay.DefaultConfig()
	config.MaxSessions = *capacity
	config.Logger = slog.New(slog.NewTextHandler(out, nil))
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
		"upload_timeout", config.UploadTimeout)
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
