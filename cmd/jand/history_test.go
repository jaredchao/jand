package main

import (
	"bytes"
	"context"
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
	if err := writeView(path, c, lines); err != nil {
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
