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

func TestQueuedGivesTheLink(t *testing.T) {
	var out, stderr bytes.Buffer
	emitter(false, &out, &stderr)(transfer.Event{Event: "queued", Code: "C0DE", Relay: "https://relay.example.com",
		Link: "https://relay.example.com/r#C0DE", ExpiresIn: 1800, Chat: "3323ff55ae57c7ea419a68f2e8de5f9c"})
	text := out.String()
	if !strings.HasPrefix(text, "Link: https://relay.example.com/r#C0DE\n  -> Give this link to the other side") ||
		!strings.Contains(text, "Claim within 30 minutes") || !strings.Contains(text, "Code: C0DE") ||
		!strings.Contains(text, "Do not send it to the other side") {
		t.Fatalf("%q", text)
	}
}

func TestShareLinks(t *testing.T) {
	for _, c := range []struct{ link, relay, code string }{
		{"https://relay.example.com/r#C0DE", "https://relay.example.com", "C0DE"},
		{"  https://relay.example.com/r/#C0DE\n", "https://relay.example.com", "C0DE"},
		{"http://203.0.113.10:8787/r#C0DE", "http://203.0.113.10:8787", "C0DE"},
		{"https://example.com/jand/r#C0DE", "https://example.com/jand", "C0DE"},
	} {
		relay, code, ok := transfer.ParseShareLink(c.link)
		if !ok || relay != c.relay || code != c.code {
			t.Errorf("%q -> %q %q %v", c.link, relay, code, ok)
		}
		if transfer.ShareLink(c.relay, c.code) != strings.TrimSpace(strings.Replace(c.link, "/r/#", "/r#", 1)) {
			t.Errorf("ShareLink(%q) round trip", c.relay)
		}
	}
	for _, notLink := range []string{"C0DE", "https://relay.example.com/#C0DE", "https://relay.example.com/r", "ftp://x/r#C0DE"} {
		if _, _, ok := transfer.ParseShareLink(notLink); ok {
			t.Errorf("%q taken as a link", notLink)
		}
	}
	o := transfer.Options{RelayURL: "https://other.example.com"}
	if _, err := fromLink("https://relay.example.com/r#C0DE", &o, true); err == nil {
		t.Error("a --relay that disagrees with the link was accepted")
	}
	o = transfer.Options{RelayURL: "https://configured.example.com"}
	if code, err := fromLink("https://relay.example.com/r#C0DE", &o, false); err != nil || code != "C0DE" || o.RelayURL != "https://relay.example.com" {
		t.Errorf("link should override the configured relay: %q %q %v", code, o.RelayURL, err)
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

func TestAccessTokenSources(t *testing.T) {
	t.Setenv("JAND_ACCESS_TOKEN", "")
	var c clientConfig
	if tok, src, err := c.accessToken(); tok != "" || src != "none" || err != nil {
		t.Fatal(tok, src, err)
	}
	file := t.TempDir() + "/token"
	os.WriteFile(file, []byte("from-file\n"), 0600)
	c.AccessTokenFile = file
	if tok, src, _ := c.accessToken(); tok != "from-file" || src != "config" {
		t.Fatal(tok, src)
	}
	t.Setenv("JAND_ACCESS_TOKEN", "from-env")
	if tok, src, _ := c.accessToken(); tok != "from-env" || src != "JAND_ACCESS_TOKEN" {
		t.Fatal(tok, src)
	}
	t.Setenv("JAND_ACCESS_TOKEN", "")
	os.WriteFile(file, []byte("  \n"), 0600)
	if _, _, err := c.accessToken(); err == nil {
		t.Fatal("empty token file accepted")
	}
}

func TestCheckpointSaysPausedNotEnded(t *testing.T) {
	var out, stderr bytes.Buffer
	emitter(false, &out, &stderr)(transfer.Event{Event: "checkpoint", By: "relay", Reason: "all_done",
		Chat: "3323ff55ae57c7ea419a68f2e8de5f9c", Message: transfer.PausedNote})
	text := out.String()
	if !strings.Contains(text, "paused, not ended") || !strings.Contains(text, "no new code is needed") ||
		!strings.Contains(text, "jand chat propose --goal '...' 3323ff55") || !strings.Contains(text, "jand chat close 3323ff55") {
		t.Fatalf("%q", text)
	}
}
