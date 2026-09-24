package main

import (
	"bufio"
	"cmp"
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
	brief := fs.Bool("brief", false, "log: one line per entry, text clipped")
	outPath := fs.String("out", "", "view: write the page here")
	noOpen := fs.Bool("no-open", false, "view: do not open a browser")
	notify := fs.Bool("notify", false, "watch: also show a desktop notification")
	all := fs.Bool("all", false, "list: include chats that ended more than a week ago")
	if err := fs.Parse(args); err != nil {
		chatUsage(stderr)
		return 2
	}
	dir := transfer.DefaultStateDir()
	if sub == "watch" {
		if fs.NArg() != 1 {
			chatUsage(stderr)
			return 2
		}
		if !*jsonOutput {
			fmt.Fprintln(out, "Watching for events your agent has not read yet. Ctrl-C stops watching; it never takes messages from the agent.")
		}
		emit := watchPrinter(out, *jsonOutput)
		if *notify {
			print := emit
			emit = func(e transfer.Event) {
				print(e)
				if title, body := watchNotice(e); title != "" {
					desktopNotify(title, body)
				}
			}
		}
		if err := transfer.ChatWatch(ctx, fs.Arg(0), 0, transfer.Options{Emit: emit}); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
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
		hidden := 0
		if !*all {
			// Old finished chats only cost whoever reads the list, often an agent.
			cutoff := time.Now().Add(-7 * 24 * time.Hour)
			kept := list[:0]
			for _, c := range list {
				if (c.Status == "ended" || c.Status == "unknown") && c.Last.Before(cutoff) {
					hidden++
					continue
				}
				kept = append(kept, c)
			}
			list = kept
		}
		printList(out, list, *jsonOutput)
		if hidden > 0 && !*jsonOutput {
			fmt.Fprintf(out, "(%d chats that ended more than a week ago are hidden; --all shows them)\n", hidden)
		}
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
		} else if *brief {
			printBriefHeader(out, summary)
			for _, l := range lines {
				printBrief(out, summary.Role, l)
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

// watchPrinter shows what the peer sent that the agent has not read, and
// rings the terminal bell so a person looking elsewhere notices.
func watchPrinter(out io.Writer, jsonOutput bool) func(transfer.Event) {
	enc := json.NewEncoder(out)
	return func(e transfer.Event) {
		if jsonOutput {
			enc.Encode(e)
			return
		}
		now := time.Now().Format("15:04:05")
		switch e.Event {
		case "message":
			tag := e.ID + " " + e.Kind
			if e.ReplyTo != "" {
				tag += " -> " + e.ReplyTo
			}
			fmt.Fprintf(out, "\a%s  %s sent %s; your agent has not read it yet\n%s\n", now, e.From, tag, indent(clip(printable(e.Text), 300)))
		case "done":
			fmt.Fprintf(out, "\a%s  %s reports its share done; your agent has not read it yet\n", now, e.From)
		case "checkpoint":
			fmt.Fprintf(out, "\a%s  checkpoint (%s): the chat is paused; ask your agent to check in with you\n", now, cmp.Or(e.Reason, "peer"))
		case "proposal":
			fmt.Fprintf(out, "\a%s  %s proposes a next goal; your agent must show it to you\n%s\n", now, e.From, indent(clip(printable(e.Goal), 300)))
		case "resumed":
			fmt.Fprintf(out, "%s  new goal accepted; the chat resumed\n", now)
		case "closed", "expired", "declined":
			fmt.Fprintf(out, "\a%s  the chat ended (%s)\n", now, cmp.Or(e.Reason, e.Event))
		case "error":
			fmt.Fprintf(out, "%s  %s\n", now, e.Message)
		default:
			fmt.Fprintf(out, "%s  %s\n", now, e.Event)
		}
	}
}

// watchNotice is the desktop notification for a watched event; an empty
// title means the event is not worth interrupting a person for.
func watchNotice(e transfer.Event) (title, body string) {
	switch e.Event {
	case "message":
		return "jand：对方发来 " + e.ID + " " + e.Kind, "你的 Agent 还没读，去叫它看一下。" + clip(oneLine(e.Text), 80)
	case "done":
		return "jand：对方报告完成", "叫你的 Agent 看一下，并核对结果。"
	case "checkpoint":
		return "jand：对话暂停了", "需要你决定结束还是继续，去问你的 Agent。"
	case "proposal":
		return "jand：对方提出了新目标", "需要你同意才会继续，去问你的 Agent。"
	case "closed", "expired", "declined":
		return "jand：对话结束了", "去问你的 Agent 结论。"
	}
	return "", ""
}

// desktopNotify shows a notification where the system offers a command for
// it: osascript on macOS, notify-send on Linux. Failure is silent: the
// terminal line and bell are still there.
func desktopNotify(title, body string) {
	switch runtime.GOOS {
	case "darwin":
		exec.Command("osascript", "-e", "display notification "+appleScriptString(body)+" with title "+appleScriptString(title)).Run()
	case "linux":
		if path, err := exec.LookPath("notify-send"); err == nil {
			exec.Command(path, title, body).Run()
		}
	}
}

// appleScriptString quotes s as an AppleScript string literal. Remote text
// reaches it, so a quote must not end the literal early.
func appleScriptString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
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

// printBriefHeader and printBrief are log --brief: what an agent reads to
// answer "how is it going", a line per entry instead of every full text.
func printBriefHeader(out io.Writer, c transfer.ChatSummary) {
	fmt.Fprintf(out, "chat %s  you=%s  %s  %d msgs  goal: %s\n", c.ID[:8], c.Role, c.Status, c.Messages, clip(oneLine(c.Goal), 80))
}

func printBrief(out io.Writer, self string, l transfer.TranscriptLine) {
	t, _ := time.Parse(time.RFC3339, l.Time)
	who := l.From
	if who == self {
		who += "*"
	}
	what := lineLabel(l)
	if l.Kind == "message" {
		what = l.ID + " " + l.Type
		if l.ReplyTo != "" {
			what += "->" + l.ReplyTo
		}
		what += ": " + clip(oneLine(l.Text), 100)
	} else if l.Kind == "done" && l.Text != "" {
		what += ": " + clip(oneLine(l.Text), 100)
	}
	fmt.Fprintf(out, "%s %s %s\n", localTime(t, "15:04"), who, what)
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

var viewTemplate = template.Must(template.New("view").Funcs(template.FuncMap{"text": printable}).Parse(viewPage))

// viewItem is one entry of the page: who did what, and how it relates to
// other entries (what it answers, what answered it, what replaced it).
type viewItem struct {
	Time, Who, Role, Kind, Type, ID, Action, Text string
	Mine, Peer, System                            bool
	ReplyTo, ReplyToWho, ReplyToQuote             string
	Answers                                       []string // replies and deliveries naming this message
	Pending                                       bool     // a request nobody has answered yet
	Supersedes, SupersededBy                      string
}

// viewSection groups the entries worked on under one goal.
type viewSection struct {
	N                   int
	Goal, Budget, Start string
	Messages            int
	Outcome             string
	Items               []*viewItem
}

type viewRequest struct {
	ID, Who, Summary, Time string
	Mine                   bool
	Answers                []string
}

type viewData struct {
	Chat                                 transfer.ChatSummary
	Short, Started, Last, Budget, Status string
	Generated, MyRole, PeerRole          string
	Sections                             []*viewSection
	Requests                             []viewRequest
	Pending                              int
}

var messageAction = map[string]string{
	"note":     "留言",
	"progress": "通报进展 · 不需要回应",
	"request":  "提出请求 · 需要对方回应",
	"reply":    "回复",
	"delivery": "交付产物 · 请对方取用并验证",
}

var statusText = map[string]string{
	"waiting": "等待对方加入", "active": "进行中", "paused": "已暂停，等待用户决定",
	"ended": "已结束", "unknown": "未知",
}

// viewAction says in words what a transcript entry did.
func viewAction(l transfer.TranscriptLine, self string) string {
	switch l.Kind {
	case "message":
		if l.Type == "reply" && l.ReplyTo != "" {
			return "回复 " + l.ReplyTo
		}
		return cmp.Or(messageAction[l.Type], l.Type)
	case "started":
		return fmt.Sprintf("发起对话，提出目标（流程 %s，预算 %s）", orDefault(l.Workflow), budgetWords(l.Budget))
	case "joined":
		if l.Text == "" && l.Workflow == "" {
			return "同意目标，加入了对话"
		}
		return fmt.Sprintf("同意目标，加入对话（流程 %s，预算 %s）", orDefault(l.Workflow), budgetWords(l.Budget))
	case "opened":
		return "领取了交接包，正在请自己的用户决定是否加入"
	case "done":
		return "报告：自己负责的部分已完成"
	case "checkpoint":
		switch l.Text {
		case "all_done":
			return "双方都已完成，对话暂停，等双方的用户决定结束还是继续"
		case "any_done":
			return "按流程，一方完成即暂停，等双方的用户验收"
		case "budget":
			return "这个目标的消息预算用完了，对话暂停，等双方的用户决定"
		}
		return "要求立即暂停，请双方的用户决定"
	case "proposal":
		return "提出下一个目标（需要双方的用户都同意）"
	case "resumed":
		return fmt.Sprintf("双方都同意了新目标，对话继续（预算 %s）", budgetWords(l.Budget))
	case "decline":
		return "拒绝了邀请"
	case "closed":
		return "结束了对话"
	case "expired":
		return "对话超时自动结束（" + l.Text + "）"
	}
	return l.Kind
}

func budgetWords(n int) string {
	if n == 0 {
		return "按 Relay 默认"
	}
	return fmt.Sprintf("%d 条", n)
}

// writeView renders the chat as one self-contained HTML page: no scripts,
// no external resources, every piece of text escaped by html/template.
func writeView(path string, c transfer.ChatSummary, lines []transfer.TranscriptLine) error {
	d := viewData{Chat: c, Short: c.ID[:8], Started: localTime(c.Started, "2006-01-02 15:04:05"),
		Last: localTime(c.Last, "2006-01-02 15:04:05"), Budget: budgetWords(c.Budget),
		Status: cmp.Or(statusText[c.Status], c.Status), Generated: time.Now().Format("2006-01-02 15:04:05"),
		MyRole: c.Role, PeerRole: "guest"}
	if c.Role == "guest" {
		d.PeerRole = "host"
	}
	who := func(role string) string {
		switch role {
		case c.Role:
			return "我方 Agent"
		case "relay":
			return "Relay"
		}
		return "对方 Agent"
	}
	byID := map[string]*viewItem{}
	var section *viewSection
	newSection := func(goal string, budget int, start string) {
		section = &viewSection{N: len(d.Sections) + 1, Goal: goal, Budget: budgetWords(budget), Start: start}
		d.Sections = append(d.Sections, section)
	}
	for _, l := range lines {
		t, _ := time.Parse(time.RFC3339, l.Time)
		if section == nil || l.Kind == "resumed" {
			goal := ""
			if l.Kind == "started" || l.Kind == "resumed" || (l.Kind == "joined" && l.Text != "") {
				goal = l.Text
			}
			if section == nil || l.Kind == "resumed" {
				newSection(goal, l.Budget, localTime(t, "2006-01-02 15:04"))
			}
		}
		it := &viewItem{Time: localTime(t, "15:04:05"), Who: who(l.From), Role: l.From, Kind: l.Kind, Type: l.Type,
			ID: l.ID, Action: viewAction(l, c.Role), Text: l.Text, ReplyTo: l.ReplyTo, Supersedes: l.Supersedes,
			Mine: l.From == c.Role, System: l.From == "relay"}
		it.Peer = !it.Mine && !it.System
		if l.Kind != "message" && !showsText(l.Kind) || l.Kind == "started" || (l.Kind == "joined" && l.Text != "") {
			it.Text = "" // the goal heads its section; reason codes are in the action
		}
		if l.Kind == "message" {
			section.Messages++
			byID[l.ID] = it
			if target := byID[l.ReplyTo]; target != nil {
				it.ReplyToWho, it.ReplyToQuote = target.Who, clip(oneLine(target.Text), 80)
				target.Answers = append(target.Answers, l.ID)
			}
			if old := byID[l.Supersedes]; old != nil {
				old.SupersededBy = l.ID
			}
		}
		if l.Kind == "checkpoint" || l.Kind == "closed" || l.Kind == "expired" {
			section.Outcome = it.Who + "：" + it.Action
		}
		section.Items = append(section.Items, it)
	}
	for _, sec := range d.Sections {
		for _, it := range sec.Items {
			if it.Kind == "message" && it.Type == "request" {
				it.Pending = len(it.Answers) == 0
				if it.Pending {
					d.Pending++
				}
				d.Requests = append(d.Requests, viewRequest{ID: it.ID, Who: it.Who, Summary: clip(oneLine(it.Text), 70),
					Time: it.Time, Mine: it.Mine, Answers: it.Answers})
			}
		}
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
