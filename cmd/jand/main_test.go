package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jaredchao/jand/internal/transfer"
)

func TestSendHelpShowsCommandUsage(t *testing.T) {
	for _, args := range [][]string{{"send", "--help"}, {"send", "-h"}, {"--json", "--help"}} {
		var out, stderr bytes.Buffer
		if got := run(context.Background(), args, &out, &stderr); got != 0 {
			t.Fatalf("%v: exit %d", args, got)
		}
		if !strings.Contains(out.String(), "jand send [options] <file>") || stderr.Len() != 0 {
			t.Fatalf("%v: out=%q stderr=%q", args, out.String(), stderr.String())
		}
	}
	var out, stderr bytes.Buffer
	if got := run(context.Background(), []string{"send", "--bogus"}, &out, &stderr); got != 2 {
		t.Fatalf("unknown flag: exit %d", got)
	}
	if !strings.Contains(stderr.String(), "jand send [options] <file>") {
		t.Fatalf("unknown flag: stderr=%q", stderr.String())
	}
}

func TestDashLeadingCodeIsNotAFlag(t *testing.T) {
	c := "-aE2VPs842phkNkt3--SBcrnzJHW0xymj1HhNbqvaPI"
	got := protectCodes([]string{"--json", "--relay", "http://x", c})
	if strings.Join(got, " ") != "--json --relay http://x -- "+c {
		t.Fatalf("%q", got)
	}
	// An unknown flag that is not a code stays a usage error.
	if got := protectCodes([]string{"--bogus"}); len(got) != 1 {
		t.Fatalf("%q", got)
	}
}

func TestChatQueuedSaysWhichIdGoesToPeer(t *testing.T) {
	var out, stderr bytes.Buffer
	emitter(false, &out, &stderr)(transfer.Event{Event: "queued", Code: "C0DE", Chat: "3323ff55ae57c7ea419a68f2e8de5f9c"})
	text := out.String()
	if !strings.Contains(text, "Code: C0DE\n  -> Give this receive code to the other side") ||
		!strings.Contains(text, "Do not send it to the other side") {
		t.Fatalf("%q", text)
	}
}

func TestFileMessagesKeepTheirBytes(t *testing.T) {
	path := t.TempDir() + "/doc.md"
	os.WriteFile(path, []byte("终稿\n"), 0600)
	if got, _ := readText(path, nil); got != "终稿\n" {
		t.Fatalf("file: %q", got)
	}
	if got, _ := readText("", strings.NewReader("stdin\n")); got != "stdin" {
		t.Fatalf("stdin: %q", got)
	}
}

func TestClientConfigPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JAND_HOME", home)
	t.Setenv("JAND_RELAY", "")
	os.WriteFile(home+"/config.json", []byte(`{"relay":"https://relay.example.com","chat":{"recv_wake":["request"],"default_budget":30}}`), 0600)
	show := func() map[string]any {
		var out, stderr bytes.Buffer
		if got := run(context.Background(), []string{"config"}, &out, &stderr); got != 0 {
			t.Fatalf("config: %d %s", got, stderr.String())
		}
		var v map[string]any
		json.Unmarshal(out.Bytes(), &v)
		return v
	}
	if v := show(); v["relay"] != "https://relay.example.com" || v["relay_source"] != "config" || v["default_budget"] != float64(30) {
		t.Fatalf("from config: %v", v)
	}
	t.Setenv("JAND_RELAY", "https://env.example.com")
	if v := show(); v["relay"] != "https://env.example.com" || v["relay_source"] != "JAND_RELAY" {
		t.Fatalf("env over config: %v", v)
	}
	os.WriteFile(home+"/config.json", []byte(`{"rlay":"x"}`), 0600)
	var out, stderr bytes.Buffer
	if got := run(context.Background(), []string{"config"}, &out, &stderr); got != 2 || !strings.Contains(stderr.String(), "rlay") {
		t.Fatalf("typo in config: %d %s", got, stderr.String())
	}
}
