package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jaredchao/jand/internal/relay"
)

// setupEnv gives a test its own home, JAND_HOME and a relay that requires
// the access token "team-secret".
func setupEnv(t *testing.T) (home, url string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("JAND_HOME", filepath.Join(home, "jand"))
	t.Setenv("JAND_RELAY", "")
	t.Setenv("JAND_ACCESS_TOKEN", "")
	cfg := relay.DefaultConfig()
	sum := sha256.Sum256([]byte("team-secret"))
	cfg.AccessTokenHashes = [][]byte{sum[:]}
	cfg.Version = version
	r := relay.New(cfg)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	t.Cleanup(r.Close)
	return home, srv.URL
}

func TestSetupNonInteractive(t *testing.T) {
	home, url := setupEnv(t)
	os.MkdirAll(filepath.Join(home, ".codex"), 0755)
	os.WriteFile(filepath.Join(home, ".codex", "AGENTS.md"), []byte("# 我的规则\n\n不要乱改。\n"), 0644)
	os.MkdirAll(filepath.Join(home, ".claude"), 0755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"model":"opus","permissions":{"allow":["Bash(ls:*)"]}}`), 0644)
	tokenFile := filepath.Join(home, "token")
	os.WriteFile(tokenFile, []byte("team-secret\n"), 0600)

	args := []string{"--yes", "--allow-claude", "--relay", url, "--token-file", tokenFile, "--agents", "claude,codex"}
	for round := 1; round <= 2; round++ { // running again must not duplicate anything
		var out, stderr bytes.Buffer
		if code := setup(context.Background(), args, strings.NewReader(""), &out, &stderr); code != 0 {
			t.Fatalf("round %d: exit %d\n%s\n%s", round, code, out.String(), stderr.String())
		}
		if !strings.Contains(out.String(), "发送、领取、回执都正常") {
			t.Fatalf("round %d: self-test not reported:\n%s", round, out.String())
		}
	}

	cfg, err := loadClientConfig()
	if err != nil || cfg.Relay != url || cfg.AccessTokenFile != tokenFile || cfg.Chat.WakeMode != "background" || cfg.OutDir != filepath.Join(home, "jand-received") {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	skill, _ := os.ReadFile(filepath.Join(home, ".claude", "skills", "jand", "SKILL.md"))
	if !strings.HasPrefix(string(skill), "---\nname: jand\n") || !strings.Contains(string(skill), "help agent") {
		t.Fatalf("skill: %s", skill)
	}
	var settings map[string]any
	data, _ := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	json.Unmarshal(data, &settings)
	allow := settings["permissions"].(map[string]any)["allow"].([]any)
	if settings["model"] != "opus" || len(allow) != 2 || allow[0] != "Bash(ls:*)" || !strings.HasPrefix(allow[1].(string), "Bash(") {
		t.Fatalf("settings: %s", data)
	}
	if backups, _ := filepath.Glob(filepath.Join(home, ".claude", "settings.json.bak-*")); len(backups) != 1 {
		t.Fatalf("expected one backup (the second run changes nothing), got %v", backups)
	}
	agents, _ := os.ReadFile(filepath.Join(home, ".codex", "AGENTS.md"))
	if !strings.HasPrefix(string(agents), "# 我的规则\n\n不要乱改。\n\n<!-- jand:begin -->") || strings.Count(string(agents), codexBegin) != 1 {
		t.Fatalf("codex AGENTS.md: %q", agents)
	}
}

func TestSetupInteractiveTypedToken(t *testing.T) {
	home, url := setupEnv(t)
	// Answers: relay, token, Claude Code yes, Codex no, others no, output
	// directory default, allow in Claude Code no. With Claude Code alone the
	// wake mode is not asked.
	input := strings.Join([]string{url, "team-secret", "y", "n", "n", "", "n"}, "\n") + "\n"
	var out, stderr bytes.Buffer
	if code := setup(context.Background(), nil, strings.NewReader(input), &out, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), stderr.String())
	}
	cfg, _ := loadClientConfig()
	info, err := os.Stat(cfg.AccessTokenFile)
	if err != nil || info.Mode().Perm() != 0600 || cfg.Chat.WakeMode != "background" || cfg.OutDir != filepath.Join(home, "jand-received") {
		t.Fatalf("token file %v %v, config %+v", info, err, cfg)
	}
	if data, _ := os.ReadFile(clientConfigPath()); strings.Contains(string(data), "team-secret") {
		t.Fatal("the token itself was written into config.json")
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); err == nil {
		t.Fatal("Claude Code permissions changed although the user said no")
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); err == nil {
		t.Fatal("Codex instructions written although the user said no")
	}
}

func TestSetupRejectsUnreachableRelay(t *testing.T) {
	setupEnv(t)
	var out, stderr bytes.Buffer
	if code := setup(context.Background(), []string{"--yes", "--relay", "http://127.0.0.1:1", "--agents", "other"}, strings.NewReader(""), &out, &stderr); code == 0 {
		t.Fatal("setup accepted a relay it could not reach")
	}
	if _, err := os.Stat(clientConfigPath()); err == nil {
		t.Fatal("config written for an unreachable relay")
	}
}

func TestSetupAsksWakeModeAsYesNo(t *testing.T) {
	_, url := setupEnv(t)
	// Claude Code no, Codex no, another agent yes: the wake question is a
	// yes/no; "n" means the agent polls.
	input := strings.Join([]string{url, "team-secret", "n", "n", "y", "n", "~/inbox"}, "\n") + "\n"
	var out, stderr bytes.Buffer
	if code := setup(context.Background(), []string{"--skip-test"}, strings.NewReader(input), &out, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out.String(), stderr.String())
	}
	if cfg, _ := loadClientConfig(); cfg.Chat.WakeMode != "poll" || cfg.OutDir != filepath.Join(os.Getenv("HOME"), "inbox") {
		t.Fatalf("wake mode %q, out dir %q", cfg.Chat.WakeMode, cfg.OutDir)
	}
	if !strings.Contains(out.String(), "(y/n) [n]") || !strings.Contains(out.String(), "系统通知") {
		t.Fatalf("output:\n%s", out.String())
	}
}
