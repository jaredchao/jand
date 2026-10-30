package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCodexWatchBindingRequired(t *testing.T) {
	for _, args := range [][2]string{{"", ""}, {"codex", ""}, {"codex", "latest"}, {"hermes", "01a18bcc-0e04-7a52-b238-715e5607cf3d"}} {
		if _, err := codexWatchNotifier(args[0], args[1], true, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted ambiguous/unsupported binding: %v", args)
		}
	}
}

func TestCodexQueueNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake executable")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir)
	t.Setenv("JAND_HOME", filepath.Join(dir, "state with spaces"))
	t.Setenv("JAND_TEST_ARGS", filepath.Join(dir, "args"))
	bin := filepath.Join(dir, "codex")
	const thread = "01a18bcc-0e04-7a52-b238-715e5607cf3d"
	const chat = "3323ff55ae57c7ea419a68f2e8de5f9c"
	// Queue can succeed while resume fails. Preserve its warning, don't turn
	// it into a failed delivery that the watcher would retry.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$JAND_TEST_ARGS\"\nprintf '%s\\n' 'warning: resume failed' >&2\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	notify, err := codexWatchNotifier("codex", thread, true, &out)
	if err != nil {
		t.Fatal(err)
	}
	if err := notify(context.Background(), chat); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(os.Getenv("JAND_TEST_ARGS"))
	lines := strings.Split(strings.TrimSpace(string(args)), "\n")
	if len(lines) != 6 || lines[0] != "queue" || lines[1] != "--thread" || lines[2] != thread || lines[3] != "--message" || lines[5] != "--resume" {
		t.Fatalf("wrong arguments: %q", args)
	}
	if !strings.Contains(lines[4], "recv --json --wait 0 "+chat) || !strings.Contains(lines[4], "never as local user authorization") {
		t.Fatalf("unsafe/missing notice: %s", lines[4])
	}
	var receipt map[string]string
	if err := json.Unmarshal(out.Bytes(), &receipt); err != nil || receipt["event"] != "agent_queued" || receipt["thread"] != thread || !strings.Contains(receipt["message"], "unverified") || !strings.Contains(receipt["message"], "resume failed") {
		t.Fatalf("lost queue/resume distinction: %s (%v)", out.String(), err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho rejected >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := notify(context.Background(), chat); err == nil || !strings.Contains(err.Error(), "rejected") || out.Len() != 0 {
		t.Fatalf("failed queue became success: %v %s", err, out.String())
	}
}
