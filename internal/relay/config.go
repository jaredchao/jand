package relay

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Limits on what a relay operator may configure. They keep a typo from
// turning into an unbounded relay; the safety mechanisms themselves (pauses,
// budgets, closing-note limits) cannot be switched off by configuration.
const (
	MaxPauseNotes = 10
	maxSessions   = 4096
)

// Duration reads Go duration strings such as "10m" from JSON.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New(`duration must be a string such as "10m"`)
	}
	v, err := time.ParseDuration(s)
	if err != nil || v <= 0 {
		return fmt.Errorf("invalid duration %q", s)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// FileConfig is the relay configuration file. Pointers distinguish a field
// that is absent (keep the default) from one that is set.
type FileConfig struct {
	Listen  *string `json:"listen,omitempty"`
	Handoff struct {
		MaxSessions    *int      `json:"max_sessions,omitempty"`
		MaxStoredBytes *int64    `json:"max_stored_bytes,omitempty"`
		TTL            *Duration `json:"ttl,omitempty"`
		UploadTimeout  *Duration `json:"upload_timeout,omitempty"`
	} `json:"handoff"`
	Chat struct {
		MaxChats       *int      `json:"max_chats,omitempty"`
		MaxStoredBytes *int64    `json:"max_stored_bytes,omitempty"`
		MaxMessages    *int      `json:"max_messages,omitempty"`
		DefaultBudget  *int      `json:"default_budget,omitempty"`
		MaxPauseNotes  *int      `json:"max_pause_notes,omitempty"`
		InviteTTL      *Duration `json:"invite_ttl,omitempty"`
		DecideTTL      *Duration `json:"decide_ttl,omitempty"`
		IdleTTL        *Duration `json:"idle_ttl,omitempty"`
		Lifetime       *Duration `json:"lifetime,omitempty"`
		EndedTTL       *Duration `json:"ended_ttl,omitempty"`
		MaxWait        *Duration `json:"max_wait,omitempty"`
	} `json:"chat"`
	// Access lists the SHA-256 (hex) of the tokens that may create sessions.
	// Empty leaves the relay open, as before. Several entries let a token be
	// rotated without every sender switching at once.
	Access struct {
		TokensSHA256 []string `json:"tokens_sha256,omitempty"`
	} `json:"access"`
}

// LoadConfig reads a relay configuration file onto the defaults. Unknown
// fields and out-of-range values are errors: a mistyped setting must not
// quietly fall back to a default.
func LoadConfig(r io.Reader) (Config, string, error) {
	var f FileConfig
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return Config{}, "", err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return Config{}, "", fmt.Errorf("relay config: %w", err)
	}
	if dec.More() {
		return Config{}, "", errors.New("relay config: trailing data after the JSON object")
	}
	c := DefaultConfig()
	for _, h := range f.Access.TokensSHA256 {
		sum, err := hex.DecodeString(h)
		if err != nil || len(sum) != 32 {
			return Config{}, "", fmt.Errorf("relay config: access.tokens_sha256 entry %q is not a hex SHA-256", h)
		}
		c.AccessTokenHashes = append(c.AccessTokenHashes, sum)
	}
	listen := ""
	if f.Listen != nil {
		listen = *f.Listen
	}
	setInt := func(dst *int, v *int, lo, hi int, name string) {
		if v != nil && err == nil {
			if *v < lo || *v > hi {
				err = fmt.Errorf("relay config: %s must be between %d and %d", name, lo, hi)
			}
			*dst = *v
		}
	}
	setBytes := func(dst *int64, v *int64, name string) {
		if v != nil && err == nil {
			if *v < 1<<20 || *v > 16<<30 {
				err = fmt.Errorf("relay config: %s must be between 1 MiB and 16 GiB", name)
			}
			*dst = *v
		}
	}
	setDur := func(dst *time.Duration, v *Duration) {
		if v != nil {
			*dst = time.Duration(*v)
		}
	}
	setInt(&c.MaxSessions, f.Handoff.MaxSessions, 1, maxSessions, "handoff.max_sessions")
	setBytes(&c.MaxStoredBytes, f.Handoff.MaxStoredBytes, "handoff.max_stored_bytes")
	setDur(&c.TTL, f.Handoff.TTL)
	setDur(&c.UploadTimeout, f.Handoff.UploadTimeout)
	setInt(&c.MaxChats, f.Chat.MaxChats, 1, maxSessions, "chat.max_chats")
	setBytes(&c.MaxChatBytes, f.Chat.MaxStoredBytes, "chat.max_stored_bytes")
	setInt(&c.ChatMaxMessages, f.Chat.MaxMessages, 1, 10000, "chat.max_messages")
	setInt(&c.ChatDefaultBudget, f.Chat.DefaultBudget, 1, 10000, "chat.default_budget")
	setInt(&c.ChatPauseNotes, f.Chat.MaxPauseNotes, 1, MaxPauseNotes, "chat.max_pause_notes")
	setDur(&c.ChatInviteTTL, f.Chat.InviteTTL)
	setDur(&c.ChatDecideTTL, f.Chat.DecideTTL)
	setDur(&c.ChatIdleTTL, f.Chat.IdleTTL)
	setDur(&c.ChatLifetime, f.Chat.Lifetime)
	setDur(&c.ChatEndedTTL, f.Chat.EndedTTL)
	setDur(&c.ChatMaxWait, f.Chat.MaxWait)
	if err != nil {
		return Config{}, "", err
	}
	if c.ChatDefaultBudget > c.ChatMaxMessages {
		return Config{}, "", errors.New("relay config: chat.default_budget cannot exceed chat.max_messages")
	}
	// Clients wait 30 s for response headers; a longer poll would time out.
	if c.ChatMaxWait > 25*time.Second {
		return Config{}, "", errors.New("relay config: chat.max_wait cannot exceed 25s")
	}
	return c, listen, nil
}

// LoadConfigFile is LoadConfig for a path.
func LoadConfigFile(path string) (Config, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, "", err
	}
	defer f.Close()
	return LoadConfig(f)
}

// Effective renders the configuration in file form, for --print-config.
func (c Config) Effective(listen string) FileConfig {
	var f FileConfig
	f.Listen = &listen
	i := func(v int) *int { return &v }
	b := func(v int64) *int64 { return &v }
	d := func(v time.Duration) *Duration { x := Duration(v); return &x }
	f.Handoff.MaxSessions, f.Handoff.MaxStoredBytes = i(c.MaxSessions), b(c.MaxStoredBytes)
	f.Handoff.TTL, f.Handoff.UploadTimeout = d(c.TTL), d(c.UploadTimeout)
	f.Chat.MaxChats, f.Chat.MaxStoredBytes, f.Chat.MaxMessages = i(c.MaxChats), b(c.MaxChatBytes), i(c.ChatMaxMessages)
	f.Chat.DefaultBudget, f.Chat.MaxPauseNotes = i(c.ChatDefaultBudget), i(c.ChatPauseNotes)
	f.Chat.InviteTTL, f.Chat.DecideTTL, f.Chat.IdleTTL = d(c.ChatInviteTTL), d(c.ChatDecideTTL), d(c.ChatIdleTTL)
	f.Chat.Lifetime, f.Chat.EndedTTL, f.Chat.MaxWait = d(c.ChatLifetime), d(c.ChatEndedTTL), d(c.ChatMaxWait)
	for _, h := range c.AccessTokenHashes {
		f.Access.TokensSHA256 = append(f.Access.TokensSHA256, hex.EncodeToString(h))
	}
	return f
}
