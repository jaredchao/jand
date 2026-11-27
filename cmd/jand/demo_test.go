package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDemoCommandRunsIsolatedAndRendersView(t *testing.T) {
	home := t.TempDir()
	t.Setenv("JAND_HOME", home)

	var out, stderr bytes.Buffer
	code := demoCommand(context.Background(), []string{"--fast", "--no-open", "--yes"}, strings.NewReader(""), &out, &stderr)
	if code != 0 {
		t.Fatalf("demo exited %d: stdout=%s stderr=%s", code, out.String(), stderr.String())
	}

	got := out.String()
	for _, want := range []string{
		"=== jand demo",
		"[1/6]",
		"[6/6]",
		"演练完成",
		"demo-view.html",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("demo output missing %q:\n%s", want, got)
		}
	}

	// Verify real $JAND_HOME was not polluted.
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("demo polluted JAND_HOME: %v", entries)
	}

	// Extract generated HTML path and verify its contents.
	var htmlPath string
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "demo-view.html") {
			htmlPath = trimmed
			break
		}
	}
	if htmlPath == "" {
		t.Fatalf("could not find demo-view.html path in output:\n%s", got)
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(htmlPath)) })

	raw, err := os.ReadFile(htmlPath)
	if err != nil {
		t.Fatalf("read generated view: %v", err)
	}
	html := string(raw)
	for _, want := range []string{
		"对话已结束",
		"users-schema-final.json",
		"display_name",
		"取代自己之前的交付",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("generated demo view missing %q", want)
		}
	}
}

func TestDemoCommandHelpAndBadArgs(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := demoCommand(context.Background(), []string{"--help"}, strings.NewReader(""), &out, &stderr); code != 0 || !strings.Contains(out.String(), "jand demo") {
		t.Fatalf("help failed: code=%d out=%q stderr=%q", code, out.String(), stderr.String())
	}
	out.Reset()
	stderr.Reset()
	if code := demoCommand(context.Background(), []string{"unexpected"}, strings.NewReader(""), &out, &stderr); code != 2 {
		t.Fatalf("expected exit 2 for bad args, got %d", code)
	}
}
