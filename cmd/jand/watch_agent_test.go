package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestCodexWatchBindingRequired(t *testing.T) {
	for _, args := range [][2]string{{"", ""}, {"codex", ""}, {"codex", "latest"}, {"hermes", "01a18bcc-0e04-7a52-b238-715e5607cf3d"}} {
		if _, err := codexWatchNotifier(args[0], args[1], true, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted ambiguous/unsupported binding: %v", args)
		}
	}
}

func TestCodexWatchFailsClosedWithoutHostProcessingReceipts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	marker := filepath.Join(dir, "must-not-queue")
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\necho queued > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	notify, err := codexWatchNotifier("codex", "01a18bcc-0e04-7a52-b238-715e5607cf3d", true, &bytes.Buffer{})
	if err == nil || notify != nil {
		t.Fatal("automatic queue remained enabled without same-host processing receipts")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("preflight invoked a queue mutation")
	}
}

func TestCodexWatchRepeatedAndConcurrentStartsPreserveManualState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JAND_HOME", home)
	chats := filepath.Join(home, "chats")
	os.Mkdir(chats, 0700)
	id := "3323ff55ae57c7ea419a68f2e8de5f9c"
	state := filepath.Join(chats, id+".cursor")
	original := []byte(`{"after":9}`)
	os.WriteFile(state, original, 0600)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out, stderr bytes.Buffer
			code := run(context.Background(), []string{"chat", "watch", "--json", "--agent", "codex", "--thread", "01a18bcc-0e04-7a52-b238-715e5607cf3d", id}, &out, &stderr)
			var event map[string]string
			if code != 1 || json.Unmarshal(out.Bytes(), &event) != nil || event["event"] != "agent_notify_blocked" || event["state"] != "host_receipt_unavailable" {
				t.Errorf("blocked outcome lost: code=%d stdout=%q stderr=%q", code, out.String(), stderr.String())
			}
		}()
	}
	wg.Wait()
	after, _ := os.ReadFile(state)
	if !bytes.Equal(original, after) {
		t.Fatal("blocked watch changed receiver cursor")
	}
	files, _ := os.ReadDir(chats)
	if len(files) != 1 {
		t.Fatal("blocked watch created a cost or transcript")
	}
	// Even an ordinary receiver cursor update cannot act as a host notice ack.
	os.WriteFile(state, []byte(`{"after":999}`), 0600)
	if notify, err := codexWatchNotifier("codex", "01a18bcc-0e04-7a52-b238-715e5607cf3d", true, &bytes.Buffer{}); err == nil || notify != nil {
		t.Fatal("receiver cursor falsely released the host queue gate")
	}
}
