package relay

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLoadAccessConfig(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	c, _, err := LoadConfig(strings.NewReader(`{"access":{"tokens_sha256":["` + hash + `"]}}`))
	if err != nil || len(c.AccessTokenHashes) != 1 {
		t.Fatal(c.AccessTokenHashes, err)
	}
	shown, _ := json.Marshal(c.Effective(""))
	if !strings.Contains(string(shown), hash) {
		t.Fatalf("print-config lost the hash: %s", shown)
	}
}

func TestLoadConfig(t *testing.T) {
	c, listen, err := LoadConfig(strings.NewReader(`{"listen":"0.0.0.0:9000",
		"handoff":{"ttl":"5m"},"chat":{"default_budget":25,"max_pause_notes":5,"idle_ttl":"2h"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if listen != "0.0.0.0:9000" || c.TTL != 5*time.Minute || c.ChatDefaultBudget != 25 || c.ChatPauseNotes != 5 ||
		c.ChatIdleTTL != 2*time.Hour || c.MaxSessions != DefaultConfig().MaxSessions {
		t.Fatalf("%+v %q", c, listen)
	}
	// What --print-config shows can be read back unchanged.
	shown, _ := json.Marshal(c.Effective(listen))
	again, listen2, err := LoadConfig(strings.NewReader(string(shown)))
	if err != nil || listen2 != listen || again.ChatIdleTTL != c.ChatIdleTTL || again.ChatDefaultBudget != 25 {
		t.Fatalf("round trip: %v %s", err, shown)
	}
	for name, bad := range map[string]string{
		"typo":            `{"chat":{"defualt_budget":25}}`,
		"range":           `{"handoff":{"max_sessions":0}}`,
		"pause ceiling":   `{"chat":{"max_pause_notes":11}}`,
		"budget over max": `{"chat":{"default_budget":300}}`,
		"poll too long":   `{"chat":{"max_wait":"40s"}}`,
		"bad duration":    `{"handoff":{"ttl":"soon"}}`,
		"number duration": `{"handoff":{"ttl":600}}`,
		"access not hash": `{"access":{"tokens_sha256":["ab"]}}`,
		"trailing":        `{} {}`,
	} {
		if _, _, err := LoadConfig(strings.NewReader(bad)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
