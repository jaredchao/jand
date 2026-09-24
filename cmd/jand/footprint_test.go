package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// installed runs a non-interactive setup in a test home and gives it some
// history: one chat transcript, one page, a chat state file, a received file.
func installed(t *testing.T) (home string) {
	t.Helper()
	home, url := setupEnv(t)
	os.MkdirAll(filepath.Join(home, ".codex"), 0755)
	os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("# 我的规则\n\n不要乱改。\n"), 0644)
	os.MkdirAll(filepath.Join(home, ".claude"), 0755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"model":"opus","permissions":{"allow":["Bash(ls:*)"]}}`), 0644)
	token := filepath.Join(home, "token")
	os.WriteFile(token, []byte("team-secret\n"), 0600)
	var out, stderr bytes.Buffer
	if code := setup(context.Background(), []string{"--yes", "--allow-claude", "--relay", url, "--token-file", token, "--agents", "claude,codex", "--skip-test"},
		strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("setup: %s %s", out.String(), stderr.String())
	}
	// Point the record at a stand-in program, not the test binary.
	rec := loadInstallRecord()
	rec.Program = filepath.Join(home, "bin", "jand")
	os.MkdirAll(filepath.Dir(rec.Program), 0755)
	os.WriteFile(rec.Program, []byte("binary"), 0755)
	rec.save()
	chats := filepath.Join(home, "jand", "chats")
	os.MkdirAll(chats, 0700)
	os.WriteFile(filepath.Join(chats, "aa.transcript.jsonl"), []byte(`{"kind":"started"}`+"\n"), 0600)
	os.WriteFile(filepath.Join(chats, "aa.html"), []byte("<html>"), 0600)
	os.WriteFile(filepath.Join(chats, "aa.json"), []byte(`{"key":"SECRET-KEY"}`), 0600)
	os.MkdirAll(filepath.Join(home, "jand-received", "x"), 0700)
	os.WriteFile(filepath.Join(home, "jand-received", "x", "file.md"), []byte("资料"), 0600)
	return home
}

func archiveNames(t *testing.T, path string) (names []string, all string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	var content strings.Builder
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		io.Copy(&content, tr)
	}
	sort.Strings(names)
	return names, content.String()
}

func TestUninstallTakesBackOnlyWhatSetupPlaced(t *testing.T) {
	home := installed(t)
	var out, stderr bytes.Buffer
	if code := uninstall([]string{"--yes"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), stderr.String())
	}
	archives, _ := filepath.Glob(filepath.Join(home, "jand-history-*.tar.gz"))
	if len(archives) != 1 {
		t.Fatalf("archives %v\n%s", archives, out.String())
	}
	names, content := archiveNames(t, archives[0])
	want := []string{"jand/chats/aa.html", "jand/chats/aa.transcript.jsonl", "jand/config.json", "jand/installed.json"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("archive holds %v, want %v", names, want)
	}
	if strings.Contains(content, "SECRET-KEY") || strings.Contains(content, "team-secret") {
		t.Fatal("archive holds a key or token")
	}
	for _, gone := range []string{"bin/jand", "jand", ".claude/skills/jand"} {
		if _, err := os.Stat(filepath.Join(home, gone)); err == nil {
			t.Errorf("%s still exists", gone)
		}
	}
	if backups, _ := filepath.Glob(filepath.Join(home, ".claude", "settings.json.bak-*")); len(backups) != 0 {
		t.Errorf("setup's backups remain: %v", backups)
	}
	var settings map[string]any
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	json.Unmarshal(data, &settings)
	allow := settings["permissions"].(map[string]any)["allow"].([]any)
	if settings["model"] != "opus" || len(allow) != 1 || allow[0] != "Bash(ls:*)" {
		t.Errorf("settings after uninstall: %s", data)
	}
	if agents, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md")); string(agents) != "# 我的规则\n\n不要乱改。\n" {
		t.Errorf("codex AGENTS.md after uninstall: %q", agents)
	}
	if _, err := os.Stat(filepath.Join(home, "jand-received", "x", "file.md")); err != nil {
		t.Error("received files were deleted without being asked")
	}
	if _, err := os.Stat(filepath.Join(home, "token")); err != nil {
		t.Error("a token file the user supplied was deleted")
	}
}

func TestUninstallCanTakeReceivedFilesIntoTheArchive(t *testing.T) {
	home := installed(t)
	var out, stderr bytes.Buffer
	if code := uninstall([]string{"--yes", "--delete-received"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	archives, _ := filepath.Glob(filepath.Join(home, "jand-history-*.tar.gz"))
	names, _ := archiveNames(t, archives[0])
	if !strings.Contains(strings.Join(names, ","), "received/x/file.md") {
		t.Fatalf("received file not archived: %v", names)
	}
	if _, err := os.Stat(filepath.Join(home, "jand-received")); err == nil {
		t.Fatal("received directory not deleted")
	}
}

func TestUninstallLeavesForeignFiles(t *testing.T) {
	home := installed(t)
	os.WriteFile(filepath.Join(home, "jand", "notes.txt"), []byte("not jand's"), 0600)
	cfg, _ := loadClientConfig()
	cfg.OutDir = home // a careless answer during setup
	saveClientConfig(cfg)
	var out, stderr bytes.Buffer
	if code := uninstall([]string{"--yes", "--delete-received", "--no-archive"}, strings.NewReader(""), &out, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(home, "jand", "notes.txt")); err != nil {
		t.Fatal("a file jand did not create was deleted")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex")); err != nil {
		t.Fatal("the home directory was treated as the received directory")
	}
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	home := installed(t)
	var out, stderr bytes.Buffer
	uninstall([]string{"--dry-run"}, strings.NewReader(""), &out, &stderr)
	if _, err := os.Stat(filepath.Join(home, "jand", "config.json")); err != nil || !strings.Contains(out.String(), "删除：") {
		t.Fatalf("dry run changed something or listed nothing:\n%s", out.String())
	}
}
