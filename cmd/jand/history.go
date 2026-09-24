package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jaredchao/jand/internal/transfer"
)

// history serves the commands that let a person see what happened in a
// chat: list, log and view. They read only this side's local state and
// transcript; nothing is fetched from the relay, and no token or key is shown.
func history(ctx context.Context, sub string, args []string, out, stderr io.Writer) int {
	fs := flag.NewFlagSet("jand chat "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {}
	jsonOutput := fs.Bool("json", false, "JSON output")
	follow := fs.Bool("follow", false, "log: keep printing new entries")
	outPath := fs.String("out", "", "view: write the page here")
	noOpen := fs.Bool("no-open", false, "view: do not open a browser")
	if err := fs.Parse(args); err != nil {
		chatUsage(stderr)
		return 2
	}
	dir := transfer.DefaultStateDir()
	if sub == "list" {
		if fs.NArg() != 0 {
			chatUsage(stderr)
			return 2
		}
		list, err := transfer.ChatList(dir)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		printList(out, list, *jsonOutput)
		return 0
	}
	if fs.NArg() != 1 {
		chatUsage(stderr)
		return 2
	}
	summary, lines, err := transfer.ChatHistory(dir, fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	switch sub {
	case "log":
		if *jsonOutput {
			enc := json.NewEncoder(out)
			for _, l := range lines {
				enc.Encode(l)
			}
		} else {
			printHeader(out, summary)
			for _, l := range lines {
				printLine(out, summary.Role, l)
			}
		}
		if *follow {
			return followLog(ctx, dir, summary, len(lines), *jsonOutput, out, stderr)
		}
		return 0
	default: // view
		path := *outPath
		if path == "" {
			path = filepath.Join(dir, summary.ID+".html")
		}
		if err := writeView(path, summary, lines); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if *jsonOutput {
			json.NewEncoder(out).Encode(map[string]string{"event": "view", "chat": summary.ID, "path": path})
		} else {
			fmt.Fprintf(out, "Wrote %s\n", path)
		}
		if !*noOpen {
			if err := openBrowser(path); err != nil {
				fmt.Fprintf(stderr, "Could not open a browser (%v); open the file yourself.\n", err)
			}
		}
		return 0
	}
}

func printList(out io.Writer, list []transfer.ChatSummary, jsonOutput bool) {
	if jsonOutput {
		enc := json.NewEncoder(out)
		for _, c := range list {
			enc.Encode(c)
		}
		return
	}
	if len(list) == 0 {
		fmt.Fprintf(out, "No chats in %s.\n", transfer.DefaultStateDir())
		return
	}
	fmt.Fprintf(out, "%-8s  %-5s  %-7s  %4s  %-16s  %s\n", "CHAT", "ROLE", "STATUS", "MSGS", "LAST", "GOAL")
	for _, c := range list {
		fmt.Fprintf(out, "%-8s  %-5s  %-7s  %4d  %-16s  %s\n", c.ID[:8], c.Role, c.Status, c.Messages, localTime(c.Last, "2006-01-02 15:04"), clip(oneLine(c.Goal), 60))
	}
}

func printHeader(out io.Writer, c transfer.ChatSummary) {
	fmt.Fprintf(out, "Chat %s  you are the %s  status: %s  relay: %s\n", c.ID[:8], c.Role, c.Status, c.Relay)
	if c.Goal != "" {
		fmt.Fprintf(out, "Goal: %s\n", printable(c.Goal))
	}
	fmt.Fprintln(out, strings.Repeat("-", 60))
}

func printLine(out io.Writer, self string, l transfer.TranscriptLine) {
	t, _ := time.Parse(time.RFC3339, l.Time)
	who := l.From
	if who == self {
		who += "*"
	}
	head := fmt.Sprintf("%s  %-6s ", localTime(t, "15:04:05"), who)
	switch l.Kind {
	case "message":
		tag := l.ID + " " + l.Type
		if l.ReplyTo != "" {
			tag += " -> " + l.ReplyTo
		}
		if l.Supersedes != "" {
			tag += " (replaces " + l.Supersedes + ")"
		}
		fmt.Fprintf(out, "%s%s\n%s\n", head, tag, indent(printable(l.Text)))
	default:
		fmt.Fprintf(out, "%s%s\n", head, lineLabel(l))
		if l.Text != "" && showsText(l.Kind) {
			fmt.Fprintf(out, "%s\n", indent(printable(l.Text)))
		}
	}
}

// showsText reports whether a line's text is worth showing: for checkpoints
// and expiry it is a reason code already in the label, and a resumed goal
// repeats the proposal just before it.
func showsText(kind string) bool {
	return kind != "checkpoint" && kind != "expired" && kind != "resumed"
}

// lineLabel says in words what a non-message transcript line means.
func lineLabel(l transfer.TranscriptLine) string {
	switch l.Kind {
	case "started":
		return fmt.Sprintf("started the chat (workflow %s, budget %s)", orDefault(l.Workflow), budgetText(l.Budget))
	case "joined":
		if l.Text == "" && l.Workflow == "" {
			return "joined"
		}
		return fmt.Sprintf("joined the chat (workflow %s, budget %s)", orDefault(l.Workflow), budgetText(l.Budget))
	case "opened":
		return "claimed the invitation packet"
	case "done":
		return "reported its share done"
	case "checkpoint":
		switch l.Text {
		case "all_done":
			return "checkpoint: both sides done; chat paused"
		case "any_done":
			return "checkpoint: one side done under an any-done workflow; chat paused"
		case "budget":
			return "checkpoint: message budget spent; chat paused"
		}
		return "checkpoint: asked to stop here; chat paused"
	case "proposal":
		return "proposed a next goal"
	case "resumed":
		return fmt.Sprintf("new goal accepted (budget %s); chat resumed", budgetText(l.Budget))
	case "decline":
		return "declined the invitation"
	case "closed":
		return "closed the chat"
	case "expired":
		return "chat expired (" + l.Text + ")"
	}
	return l.Kind
}

// followLog prints entries appended after the first n until the chat ends
// or the command is interrupted. The transcript grows when either side's
// jand sends or receives, so it shows what this side has seen so far.
func followLog(ctx context.Context, dir string, c transfer.ChatSummary, n int, jsonOutput bool, out, stderr io.Writer) int {
	path, err := transfer.TranscriptPath(dir, c.ID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	var offset int64
	seen := 0
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	enc := json.NewEncoder(out)
	for {
		if f, err := os.Open(path); err == nil {
			f.Seek(offset, io.SeekStart)
			r := bufio.NewReader(f)
			for {
				b, err := r.ReadBytes('\n')
				if err != nil {
					break // a partial line is read again next time
				}
				offset += int64(len(b))
				l, ok := transfer.ParseTranscriptLine(b)
				if !ok {
					continue
				}
				if seen++; seen <= n {
					continue
				}
				if jsonOutput {
					enc.Encode(l)
				} else {
					printLine(out, c.Role, l)
				}
				if l.Kind == "closed" || l.Kind == "expired" || l.Kind == "decline" {
					f.Close()
					return 0
				}
			}
			f.Close()
		}
		select {
		case <-ctx.Done():
			return 0
		case <-tick.C:
		}
	}
}

//go:embed view.html
var viewPage string

var viewTemplate = template.Must(template.New("view").Funcs(template.FuncMap{"lines": func(s string) string { return printable(s) }}).Parse(viewPage))

type viewItem struct {
	Time, From, Kind, Type, ID, ReplyTo, Supersedes, Label, Text string
	Mine, Peer, System                                           bool
}

type viewData struct {
	Chat                         transfer.ChatSummary
	Short, Started, Last, Budget string
	Generated                    string
	Items                        []viewItem
	Peer                         string
}

// writeView renders the chat as one self-contained HTML page: no scripts,
// no external resources, every piece of text escaped by html/template.
func writeView(path string, c transfer.ChatSummary, lines []transfer.TranscriptLine) error {
	d := viewData{Chat: c, Short: c.ID[:8], Started: localTime(c.Started, "2006-01-02 15:04:05"),
		Last: localTime(c.Last, "2006-01-02 15:04:05"), Budget: budgetText(c.Budget),
		Generated: time.Now().Format("2006-01-02 15:04:05"), Peer: "guest"}
	if c.Role == "guest" {
		d.Peer = "host"
	}
	for _, l := range lines {
		t, _ := time.Parse(time.RFC3339, l.Time)
		it := viewItem{Time: localTime(t, "15:04:05"), From: l.From, Kind: l.Kind, Type: l.Type, ID: l.ID,
			ReplyTo: l.ReplyTo, Supersedes: l.Supersedes, Text: l.Text,
			Mine: l.From == c.Role, System: l.From == "relay"}
		it.Peer = !it.Mine && !it.System
		if l.Kind != "message" {
			it.Label = lineLabel(l)
			if !showsText(l.Kind) {
				it.Text = ""
			}
		}
		d.Items = append(d.Items, it)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if err := viewTemplate.Execute(f, d); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func openBrowser(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		if _, err := exec.LookPath("xdg-open"); err != nil {
			return errors.New("xdg-open not found")
		}
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

func localTime(t time.Time, layout string) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format(layout)
}

func budgetText(n int) string {
	if n == 0 {
		return "relay default"
	}
	return fmt.Sprint(n)
}

func orDefault(w string) string {
	if w == "" {
		return "default"
	}
	return w
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(printable(s)), " ")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func indent(s string) string {
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}
