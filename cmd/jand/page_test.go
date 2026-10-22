package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/transfer"
)

func TestMain(m *testing.M) {
	openPage = func(string) error { return nil } // tests never start a browser
	os.Exit(m.Run())
}

func TestPageMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JAND_HOME", home)
	t.Setenv("JAND_PAGE", "")
	if got := (clientConfig{}).pageMode(); got != "open" {
		t.Fatalf("default %q", got)
	}
	var c clientConfig
	c.Chat.Page = "off"
	if got := c.pageMode(); got != "off" {
		t.Fatalf("config %q", got)
	}
	t.Setenv("JAND_PAGE", "open")
	if got := c.pageMode(); got != "open" {
		t.Fatalf("env over config %q", got)
	}
	t.Setenv("JAND_PAGE", "yes please")
	if got := c.pageMode(); got != "update" {
		t.Fatalf("unknown env value %q must not open a browser", got)
	}
	t.Setenv("JAND_PAGE", "")
	os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"chat":{"page":"always"}}`), 0600)
	var out, stderr bytes.Buffer
	if got := run(context.Background(), []string{"config"}, &out, &stderr); got != 2 || !strings.Contains(stderr.String(), "chat.page") {
		t.Fatalf("bad chat.page accepted: %d %s", got, stderr.String())
	}
}

func renderView(t *testing.T, c transfer.ChatSummary, lines []transfer.TranscriptLine) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "view.html")
	if err := writeView(path, c, lines, nil, os.ErrNotExist); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(path + ".*.tmp"); len(matches) > 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
	page, _ := os.ReadFile(path)
	return string(page)
}

func TestViewHeadsWithWhereTheChatStands(t *testing.T) {
	c := transfer.ChatSummary{ID: "3323ff55ae57c7ea419a68f2e8de5f9c", Role: "host", Goal: "对齐口径", Status: "paused",
		Started: time.Now(), Last: time.Now()}
	lines := []transfer.TranscriptLine{
		{Time: "2026-10-21T11:04:00Z", From: "host", Kind: "started", Text: "对齐口径"},
		{Time: "2026-10-21T11:05:00Z", From: "guest", Kind: "joined"},
		{Time: "2026-10-21T11:06:00Z", From: "host", Kind: "message", ID: "h1", Type: "delivery", Text: "# 口径草稿\n正文"},
		{Time: "2026-10-21T11:07:00Z", From: "host", Kind: "message", ID: "h2", Type: "delivery", Supersedes: "h1", Text: "# 确认版口径\n正文"},
		{Time: "2026-10-21T11:08:00Z", From: "guest", Kind: "message", ID: "g1", Type: "delivery", Text: "classify.py"},
		{Time: "2026-10-21T11:09:00Z", From: "guest", Kind: "done", Text: "核对了 9 条，全部接受"},
		{Time: "2026-10-21T11:09:30Z", From: "host", Kind: "done", Text: "口径定稿"},
		{Time: "2026-10-21T11:10:00Z", From: "relay", Kind: "checkpoint", Text: "all_done"},
	}

	page := renderView(t, c, lines)
	for _, want := range []string{"这一轮停下了，等你决定", `http-equiv="refresh"`, "核对了 9 条，全部接受", "口径定稿",
		`href="#h2"`, "# 确认版口径", `href="#g1"`} {
		if !strings.Contains(page, want) {
			t.Errorf("paused page lacks %q", want)
		}
	}
	results := page[strings.Index(page, "<h2>结果</h2>"):strings.Index(page, `<div class="legend">`)]
	if strings.Contains(results, "口径草稿") {
		t.Error("a superseded delivery is listed among the results")
	}

	// A proposal from the peer is the reader's decision; one from us is not.
	peer := append(lines, transfer.TranscriptLine{Time: "2026-10-21T11:11:00Z", From: "guest", Kind: "proposal", Text: "各自重跑"})
	if page := renderView(t, c, peer); !strings.Contains(page, "对方提出了下一个目标，要你同意") {
		t.Error("peer proposal not put to the reader")
	}
	mine := append(lines, transfer.TranscriptLine{Time: "2026-10-21T11:11:00Z", From: "host", Kind: "proposal", Text: "各自重跑"})
	if page := renderView(t, c, mine); !strings.Contains(page, "等对方的用户同意") {
		t.Error("own proposal shown as the reader's decision")
	}

	// A chat that can no longer change stops reloading.
	c.Status = "ended"
	ended := append(lines, transfer.TranscriptLine{Time: "2026-10-21T11:12:00Z", From: "host", Kind: "closed"})
	if page := renderView(t, c, ended); strings.Contains(page, `http-equiv="refresh"`) || !strings.Contains(page, "对话已结束") {
		t.Error("ended chat still reloads or is not marked ended")
	}

	// A chat whose guest just joined has no transcript yet beyond the join.
	c = transfer.ChatSummary{ID: c.ID, Role: "guest", Status: "active"}
	if page := renderView(t, c, nil); !strings.Contains(page, "两边的 Agent 正在协作") {
		t.Error("active chat without entries")
	}
}
