package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/jaredchao/jand/internal/transfer"
)

const defaultRelay = "http://127.0.0.1:8787"

// clientConfig is $JAND_HOME/config.json: this machine's defaults. It never
// changes the protocol or what the peer sees; flags and environment
// variables override it.
type clientConfig struct {
	Relay  string `json:"relay,omitempty"`
	OutDir string `json:"out_dir,omitempty"`
	// Reserved for relay access tokens, not implemented yet; rejected when
	// set so nobody believes a token is being sent.
	AccessTokenFile string `json:"access_token_file,omitempty"`
	Chat            struct {
		DefaultBudget   int      `json:"default_budget,omitempty"`
		RecvWake        []string `json:"recv_wake,omitempty"`
		DefaultWorkflow string   `json:"default_workflow,omitempty"`
	} `json:"chat"`
}

func clientConfigPath() string {
	return filepath.Join(transfer.HomeDir(), "config.json")
}

// loadClientConfig reads the config file; a missing file is an empty config.
func loadClientConfig() (clientConfig, error) {
	var c clientConfig
	data, err := os.ReadFile(clientConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("%s: %w", clientConfigPath(), err)
	}
	if c.AccessTokenFile != "" {
		return c, fmt.Errorf("%s: access_token_file is reserved; relay access tokens are not implemented yet", clientConfigPath())
	}
	if c.Chat.DefaultBudget < 0 || c.Chat.DefaultBudget > transfer.MaxChatBudget {
		return c, fmt.Errorf("%s: chat.default_budget must be between 1 and %d", clientConfigPath(), transfer.MaxChatBudget)
	}
	for _, k := range c.Chat.RecvWake {
		if !slices.Contains(transfer.MessageKinds, k) {
			return c, fmt.Errorf("%s: unknown message kind %q in chat.recv_wake", clientConfigPath(), k)
		}
	}
	return c, nil
}

// relayURL applies the precedence flag > JAND_RELAY > config > default.
func (c clientConfig) relayURL(flagValue string, flagSet bool) (string, string) {
	switch {
	case flagSet:
		return flagValue, "--relay"
	case os.Getenv("JAND_RELAY") != "":
		return os.Getenv("JAND_RELAY"), "JAND_RELAY"
	case c.Relay != "":
		return c.Relay, "config"
	}
	return defaultRelay, "default"
}

// configCommand prints where each effective client setting comes from.
func configCommand(args []string, out, stderr io.Writer) int {
	if len(args) > 0 && args[0] != "--json" {
		fmt.Fprintln(stderr, "usage: jand config [--json]   (prints the effective client configuration)")
		return 2
	}
	c, err := loadClientConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	relay, source := c.relayURL("", false)
	outDir, outSource := "received", "default"
	if c.OutDir != "" {
		outDir, outSource = c.OutDir, "config"
	}
	view := map[string]any{
		"config_file":      clientConfigPath(),
		"state_dir":        transfer.DefaultStateDir(),
		"workflow_dir":     transfer.WorkflowDir(),
		"relay":            relay,
		"relay_source":     source,
		"out_dir":          outDir,
		"out_dir_source":   outSource,
		"default_budget":   c.Chat.DefaultBudget,
		"recv_wake":        c.Chat.RecvWake,
		"default_workflow": c.Chat.DefaultWorkflow,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	enc.Encode(view)
	return 0
}
