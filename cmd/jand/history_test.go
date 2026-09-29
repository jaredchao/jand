package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/transfer"
)

func TestViewEscapesRemoteText(t *testing.T) {
	c := transfer.ChatSummary{ID: "3323ff55ae57c7ea419a68f2e8de5f9c", Role: "host", Relay: "https://relay.example.com",
		Goal: "对齐字段", Status: "active", Started: time.Now(), Last: time.Now(), Messages: 1}
	lines := []transfer.TranscriptLine{
		{Time: "2026-09-24T01:00:00Z", From: "host", Kind: "started", Text: "对齐字段", Workflow: "default"},
		{Time: "2026-09-24T01:01:00Z", From: "guest", Kind: "message", ID: "g1", Type: "request", Text: `<script>alert(1)</script><img src=x onerror=alert(2)>`},
		{Time: "2026-09-24T01:02:00Z", From: "host", Kind: "message", ID: "h1", Type: "reply", ReplyTo: "g1", Text: "ok"},
	}
	path := filepath.Join(t.TempDir(), "view.html")
	if err := writeView(path, c, lines, nil, os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(path)
	text := string(page)
	if strings.Contains(text, "<script>") || strings.Contains(text, "<img") || !strings.Contains(text, "&lt;script&gt;") {
		t.Fatalf("remote text not escaped")
	}
	if !strings.Contains(text, `href="#g1"`) || !strings.Contains(text, "default-src 'none'") {
		t.Fatalf("page structure: reply link or content policy missing")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0600 {
		t.Fatalf("page mode %v", info.Mode().Perm())
	}
}

func TestLogFollowStopsWhenChatEnds(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JAND_HOME", home)
	dir := transfer.DefaultStateDir()
	os.MkdirAll(dir, 0700)
	id := "3323ff55ae57c7ea419a68f2e8de5f9c"
	os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"id":"`+id+`","relay":"x","role":"host","token":"t","key":"k"}`), 0600)
	transcript := filepath.Join(dir, id+".transcript.jsonl")
	os.WriteFile(transcript, []byte(`{"time":"2026-09-24T01:00:00Z","from":"host","kind":"started","text":"g"}`+"\n"), 0600)

	var out, stderr bytes.Buffer
	done := make(chan int, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { done <- history(ctx, "log", []string{"--follow", id[:8]}, &out, &stderr) }()
	time.Sleep(300 * time.Millisecond)
	f, _ := os.OpenFile(transcript, os.O_WRONLY|os.O_APPEND, 0600)
	f.WriteString(`{"time":"2026-09-24T01:01:00Z","from":"guest","kind":"message","id":"g1","type":"note","text":"hello"}` + "\n")
	f.WriteString(`{"time":"2026-09-24T01:02:00Z","from":"guest","kind":"closed"}` + "\n")
	f.Close()
	select {
	case code := <-done:
		if code != 0 || strings.Count(out.String(), "hello") != 1 || !strings.Contains(out.String(), "closed the chat") {
			t.Fatalf("code %d, output %q %q", code, out.String(), stderr.String())
		}
	case <-ctx.Done():
		t.Fatal("log --follow did not stop after the chat closed")
	}
}

func TestWatchNotices(t *testing.T) {
	if title, _ := watchNotice(transfer.Event{Event: "message", ID: "g2", Kind: "request", Text: "字段？"}); title != "jand：对方发来 g2 request" {
		t.Fatalf("message notice %q", title)
	}
	if title, _ := watchNotice(transfer.Event{Event: "resumed"}); title != "" {
		t.Fatal("resumed should not interrupt anyone")
	}
}

func TestAppleScriptQuoting(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("osascript is macOS only")
	}
	for _, s := range []string{`plain`, `say "hi"`, `back\slash`, `"; do shell script "touch /tmp/x`, "中文 \\\" 混合"} {
		out, err := exec.Command("osascript", "-e", "return "+appleScriptString(s)).Output()
		if err != nil || strings.TrimRight(string(out), "\n") != s {
			t.Errorf("%q came back as %q (%v)", s, out, err)
		}
	}
}

func TestLogBriefIsOneLinePerEntry(t *testing.T) {
	var out bytes.Buffer
	long := strings.Repeat("很长的报告内容。", 100)
	printBrief(&out, "host", transfer.TranscriptLine{Time: "2026-09-24T01:00:00Z", From: "guest", Kind: "message", ID: "g1", Type: "delivery", Text: long + "\n第二行"})
	printBrief(&out, "host", transfer.TranscriptLine{Time: "2026-09-24T01:01:00Z", From: "host", Kind: "message", ID: "h1", Type: "reply", ReplyTo: "g1", Text: "收到"})
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 || len([]rune(lines[0])) > 140 || !strings.Contains(lines[0], "g1 delivery: ") || !strings.Contains(lines[1], "host* h1 reply->g1: 收到") {
		t.Fatalf("%q", out.String())
	}
}

func TestViewCost(t *testing.T) {
	c := transfer.ChatSummary{ID: "3323ff55ae57c7ea419a68f2e8de5f9c", Role: "host", Status: "ended",
		Started: time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC), Last: time.Date(2026, 9, 28, 1, 10, 0, 0, time.UTC)}
	lines := []transfer.TranscriptLine{
		{Time: "2026-09-28T01:00:00Z", From: "host", Kind: "started", Text: "目标一"},
		{Time: "2026-09-28T01:01:00Z", From: "guest", Kind: "message", ID: "g1", Type: "note", Text: "hi"},
		{Time: "2026-09-28T01:05:00Z", From: "relay", Kind: "resumed", Text: "目标二"},
		{Time: "2026-09-28T01:06:00Z", From: "host", Kind: "message", ID: "h1", Type: "note", Text: "ok"},
		{Time: "2026-09-28T01:10:00Z", From: "guest", Kind: "closed"},
	}
	recs := []transfer.CostRecord{
		{Time: "2026-09-28T01:00:00Z", Cmd: "start", Out: 300, Wrote: 20},
		{Time: "2026-09-28T01:01:00Z", Cmd: "recv", Out: 100},
		{Time: "2026-09-28T01:02:00Z", Cmd: "recv", Out: 20, Empty: true},
		{Time: "2026-09-28T01:03:00Z", Cmd: "recv", Out: 10, Check: true, Empty: true},
		{Time: "2026-09-28T01:05:00Z", Cmd: "accept", Out: 50},
		{Time: "2026-09-28T01:06:00Z", Cmd: "send", Out: 40, Wrote: 2},
		{Time: "2026-09-28T01:07:00Z", Cmd: "send", Out: 30, Exit: 5},
		{Time: "2026-09-28T01:11:00Z", Cmd: "recv", Out: 5, Own: true},
		{Time: "2026-09-28T01:12:00Z", Cmd: "recv", Out: 60, Exit: 4},
	}
	render := func(recs []transfer.CostRecord, rerr error) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "view.html")
		if err := writeView(path, c, lines, recs, rerr); err != nil {
			t.Fatal(err)
		}
		page, _ := os.ReadFile(path)
		return string(page)
	}

	// Worked by hand: goal 1 holds start, two waiting recvs (one empty) and a
	// recv without --wait, which is looking, not a wake; goal 2 starts at
	// 01:05 and holds accept, both sends, a recv that saw only our own close
	// (not a wake) and the recv that ended it. Bytes count for all of them.
	// The chat's time runs to the last record, 01:12, past the transcript's end.
	var secs []*viewSection
	for i, at := range []time.Time{time.Date(2026, 9, 28, 1, 0, 0, 0, time.UTC), time.Date(2026, 9, 28, 1, 5, 0, 0, time.UTC)} {
		secs = append(secs, &viewSection{start: at, N: i + 1, Messages: 1})
	}
	v := buildCost(recs, nil, secs, c.Last)
	want := []costRow{
		{Label: "整个对话", Wakes: 3, Empty: 1, Decisions: 1, Messages: 2, Out: "615 B", Wrote: "22 B", Wall: "12 分 0 秒"},
		{Label: "目标 1", Wakes: 2, Empty: 1, Decisions: 0, Messages: 1, Out: "430 B", Wrote: "20 B", Wall: "5 分 0 秒"},
		{Label: "目标 2", Wakes: 1, Empty: 0, Decisions: 1, Messages: 1, Out: "185 B", Wrote: "2 B", Wall: "7 分 0 秒"},
	}
	if v.State != "" || len(v.Rows) != 3 {
		t.Fatalf("state %q rows %d", v.State, len(v.Rows))
	}
	for i := range want {
		if v.Rows[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, v.Rows[i], want[i])
		}
	}
	const unrecorded = `<td class="muted">未记录</td>`
	if page := render(recs, nil); !strings.Contains(page, "本侧消耗") || strings.Contains(page, unrecorded) || !strings.Contains(page, "3（1）") {
		t.Fatal("whole record: card missing or marked unrecorded")
	}

	// Missing, partial and damaged records never show as zeros.
	if page := render(nil, os.ErrNotExist); !strings.Contains(page, unrecorded) || strings.Contains(page, "0（0）") {
		t.Fatal("chat before 0.4.4 shown with numbers")
	}
	if page := render(recs[1:], nil); !strings.Contains(page, "只记录了") {
		t.Fatal("record starting mid-chat not marked partial")
	}
	// The receiving side is whole from its claim; a record that starts at
	// join missed the claim and is partial.
	if v := buildCost([]transfer.CostRecord{{Time: "2026-09-28T01:00:00Z", Cmd: "claim"}}, nil, secs, c.Last); v.State != "" {
		t.Fatalf("claim-first record is %q", v.State)
	}
	if v := buildCost([]transfer.CostRecord{{Time: "2026-09-28T01:00:00Z", Cmd: "join"}}, nil, secs, c.Last); v.State != "partial" {
		t.Fatalf("join-first record is %q", v.State)
	}
	if page := render(nil, errors.New("cost record line 3 is unreadable")); !strings.Contains(page, "读不出来") || !strings.Contains(page, unrecorded) {
		t.Fatal("damaged record not marked")
	}
}

func TestMeterOwnOnlyForOurOwnActions(t *testing.T) {
	for _, c := range []struct {
		events []transfer.Event
		own    bool
	}{
		{[]transfer.Event{{Event: "closed", By: "self"}}, true},
		{[]transfer.Event{{Event: "closed", By: "peer"}}, false},
		{[]transfer.Event{{Event: "closed", By: "self"}, {Event: "message", From: "host"}}, false},
		{nil, false},
	} {
		m := chatMeter{events: c.events}
		if m.own() != c.own {
			t.Fatalf("%+v: own %v", c.events, m.own())
		}
	}
}

func TestViewMarksPlannedAndAutomaticGoals(t *testing.T) {
	c := transfer.ChatSummary{ID: "3323ff55ae57c7ea419a68f2e8de5f9c", Role: "guest", Relay: "https://relay.example.com",
		Goal: "查内存", Status: "active", Started: time.Now(), Last: time.Now()}
	plan := []transfer.PlanStep{{Goal: "查内存"}, {Goal: "汇总"}}
	lines := []transfer.TranscriptLine{
		{Time: "2026-09-29T01:00:00Z", From: "guest", Kind: "joined", Text: "查系统版本", Plan: plan, Mode: "5m"},
		{Time: "2026-09-29T01:01:00Z", From: "relay", Kind: "checkpoint", Text: "all_done"},
		{Time: "2026-09-29T01:02:00Z", From: "host", Kind: "proposal", Text: "查内存"},
		{Time: "2026-09-29T01:07:00Z", From: "guest", Kind: "advanced", Text: "查内存", Step: 2, Mode: "5m"},
		{Time: "2026-09-29T01:07:01Z", From: "relay", Kind: "resumed", Text: "查内存"},
		{Time: "2026-09-29T01:08:00Z", From: "relay", Kind: "checkpoint", Text: "all_done"},
		{Time: "2026-09-29T01:09:00Z", From: "host", Kind: "proposal", Text: "顺便查磁盘"},
		{Time: "2026-09-29T01:10:00Z", From: "relay", Kind: "resumed", Text: "顺便查磁盘"},
	}
	path := filepath.Join(t.TempDir(), "view.html")
	if err := writeView(path, c, lines, nil, os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	page, _ := os.ReadFile(path)
	text := string(page)
	for _, want := range []string{"附计划 2 个后续目标，本方推进方式：等用户 5m，没答复就推进", "2. 查内存",
		"目标 2（计划中的目标 2）", "按预授权推进（等用户 5m，没答复就推进）", "没有再问用户，推进到计划中的目标 2"} {
		if !strings.Contains(text, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// The unplanned third goal is neither marked planned nor automatic.
	if strings.Count(text, "按预授权推进（") != 1 || strings.Contains(text, "目标 3（计划中") {
		t.Error("an unplanned goal is marked as planned or automatic")
	}
	var out bytes.Buffer
	for _, l := range lines {
		printLine(&out, "guest", l)
	}
	if !strings.Contains(out.String(), "plan of 2 more goals, advance 5m") || !strings.Contains(out.String(), "took plan goal 2 without asking again") {
		t.Fatalf("log: %s", out.String())
	}
}
