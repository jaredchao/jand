// Package relay holds bounded, expiring ciphertext in memory. It never
// receives a transfer code or a file encryption key.
package relay

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jaredchao/jand/internal/code"
)

const MaxBlob = 10*1024*1024 + 4096

type Config struct {
	MaxSessions    int
	MaxStoredBytes int64
	TTL            time.Duration
	// UploadTimeout bounds how long one upload may take to deliver its body.
	// Without it a client trickling bytes holds an upload slot indefinitely.
	UploadTimeout time.Duration
	// Logger records session lifecycle events. Nil disables logging, which is
	// what tests and library users get by default.
	Logger *slog.Logger

	// Chat limits. See docs/CHAT_DESIGN.md for what each timeout means.
	MaxChats        int
	MaxChatBytes    int64
	ChatMaxMessages int // over a whole chat; each goal's budget is at most this
	// ChatDefaultBudget is the message budget of a goal whose creator gave none.
	ChatDefaultBudget int
	ChatInviteTTL     time.Duration // created, packet not claimed
	ChatDecideTTL     time.Duration // claimed, guest has not joined or declined
	ChatIdleTTL       time.Duration // joined, no message
	ChatLifetime      time.Duration
	ChatEndedTTL      time.Duration // how long the end event stays readable
	ChatMaxWait       time.Duration // longest single long-poll
	// ChatPauseNotes is how many closing messages each side may send while
	// the chat is paused, outside the budget, to tie up loose ends.
	ChatPauseNotes int

	// Version is the program version reported by /healthz; empty omits it.
	Version string

	// AccessTokenHashes are the SHA-256 of the tokens allowed to create a
	// handoff or a chat. Empty means anyone may. Only creation is checked:
	// that is what consumes relay capacity, and a receiver holding a code
	// needs no token of their own.
	AccessTokenHashes [][]byte
}

// AccessHeader carries a relay access token on requests that create sessions.
const AccessHeader = "X-Jand-Access"

// admitted reports whether req may create a session on this relay.
func (r *Relay) admitted(req *http.Request) bool {
	if len(r.config.AccessTokenHashes) == 0 {
		return true
	}
	token := req.Header.Get(AccessHeader)
	if token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	ok := 0
	for _, h := range r.config.AccessTokenHashes {
		ok |= subtle.ConstantTimeCompare(sum[:], h)
	}
	return ok == 1
}

// refuse answers a creation request that carries no valid access token.
func (r *Relay) refuse(w http.ResponseWriter, room, what string) {
	r.log.Warn(what+" rejected", "room", tag(room), "reason", "access token missing or unknown")
	http.Error(w, "this relay requires an access token; set JAND_ACCESS_TOKEN", http.StatusUnauthorized)
}

// ChatFeatures lists what this relay's chat protocol supports, reported by
// /healthz. The chat encryption label has stayed jand-chat/1 through
// incompatible releases, so a protocol number would suggest compatibility
// that does not exist; named features say what a client can rely on.
var ChatFeatures = []string{"goal", "budget", "kinds", "done", "checkpoint", "closing_notes", "charter", "done_rule_any"}

func DefaultConfig() Config {
	// A receive code passes through people (chat apps, a colleague in a
	// meeting), so it lives 30 minutes; a chat invitation outlives its packet.
	return Config{MaxSessions: 128, MaxStoredBytes: 64 * 1024 * 1024, TTL: 30 * time.Minute, UploadTimeout: 2 * time.Minute,
		MaxChats: 64, MaxChatBytes: 16 * 1024 * 1024, ChatMaxMessages: 200, ChatDefaultBudget: 40,
		ChatInviteTTL: 35 * time.Minute, ChatDecideTTL: 30 * time.Minute, ChatIdleTTL: 60 * time.Minute,
		ChatLifetime: 6 * time.Hour, ChatEndedTTL: 10 * time.Minute, ChatMaxWait: 20 * time.Second,
		ChatPauseNotes: 3}
}

type session struct {
	claim, status string
	blob          []byte
	receipt       string
	claimed       bool
	expires       time.Time
}

type Relay struct {
	config  Config
	log     *slog.Logger
	started time.Time
	mu      sync.Mutex
	rooms   map[string]*session
	stored  int64
	chats   map[string]*chatRoom
	// chatBytes counts queued chat ciphertext, separately from handoff blobs.
	chatBytes int64
	closed    bool
	uploads   chan struct{}
}

func New(config Config) *Relay {
	d := DefaultConfig()
	if config.MaxSessions <= 0 {
		config.MaxSessions = d.MaxSessions
	}
	if config.MaxStoredBytes <= 0 {
		config.MaxStoredBytes = d.MaxStoredBytes
	}
	if config.TTL <= 0 {
		config.TTL = d.TTL
	}
	if config.UploadTimeout <= 0 {
		config.UploadTimeout = d.UploadTimeout
	}
	positive(&config.MaxChats, d.MaxChats)
	positive(&config.MaxChatBytes, d.MaxChatBytes)
	positive(&config.ChatMaxMessages, d.ChatMaxMessages)
	positive(&config.ChatDefaultBudget, d.ChatDefaultBudget)
	config.ChatDefaultBudget = min(config.ChatDefaultBudget, config.ChatMaxMessages)
	positive(&config.ChatInviteTTL, d.ChatInviteTTL)
	positive(&config.ChatDecideTTL, d.ChatDecideTTL)
	positive(&config.ChatIdleTTL, d.ChatIdleTTL)
	positive(&config.ChatLifetime, d.ChatLifetime)
	positive(&config.ChatEndedTTL, d.ChatEndedTTL)
	positive(&config.ChatMaxWait, d.ChatMaxWait)
	positive(&config.ChatPauseNotes, d.ChatPauseNotes)
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Relay{config: config, log: logger, started: time.Now(),
		rooms: make(map[string]*session), chats: make(map[string]*chatRoom), uploads: make(chan struct{}, min(8, config.MaxSessions))}
}

// tag identifies a session in logs without reproducing the full room id.
// The room is already visible in any reverse-proxy access log; this keeps the
// relay's own log useful for correlation without widening that exposure.
func tag(room string) string {
	if len(room) > 8 {
		return room[:8]
	}
	return room
}

// levelsLocked reports the current water marks for log lines. Caller holds mu.
func (r *Relay) levelsLocked() (int, int64) {
	return len(r.rooms), r.stored
}

func (r *Relay) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	return len(r.rooms)
}

func (r *Relay) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	clear(r.rooms)
	r.stored = 0
	for _, c := range r.chats {
		c.wake()
	}
	clear(r.chats)
	r.chatBytes = 0
}

func positive[T int | int64 | time.Duration](v *T, fallback T) {
	if *v <= 0 {
		*v = fallback
	}
}

func (r *Relay) expireLocked() {
	now := time.Now()
	for room, s := range r.rooms {
		if !now.Before(s.expires) {
			r.stored -= int64(len(s.blob))
			delete(r.rooms, room)
			r.log.Info("session expired", "room", tag(room), "bytes", len(s.blob), "claimed", s.claimed)
		}
	}
}

func validToken(v string) bool {
	b, err := base64.RawURLEncoding.DecodeString(v)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == v
}

func equal(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.URL.Path == "/" && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		// A 404 here read as "the relay is down" to someone probing it.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "jand relay %s\nIt passes end-to-end encrypted handoffs and chats between jand clients; it cannot read them.\nHealth: /healthz\n", r.config.Version)
		return
	}
	if (req.URL.Path == "/r" || req.URL.Path == "/r/") && (req.Method == http.MethodGet || req.Method == http.MethodHead) {
		// Where a share link lands when someone opens it in a browser. The
		// code after # never reaches the server, so this page cannot show it.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		io.WriteString(w, linkPage)
		return
	}
	if req.URL.Path == "/healthz" && req.Method == http.MethodGet {
		// Body added so an operator can tell a live process from a stale proxy
		// cache or a listener that accepts but no longer serves, and which
		// release answers. Session counts are deliberately omitted: this
		// endpoint is unauthenticated.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(struct {
			Status        string   `json:"status"`
			UptimeSeconds int64    `json:"uptime_seconds"`
			Version       string   `json:"version,omitempty"`
			Handoff       string   `json:"handoff"`
			ChatFeatures  []string `json:"chat_features"`
			Access        bool     `json:"access"`
		}{"ok", int64(time.Since(r.started).Seconds()), r.config.Version, "handoff/0.2", ChatFeatures, len(r.config.AccessTokenHashes) > 0})
		return
	}
	if chat, ok := strings.CutPrefix(req.URL.Path, "/v1/chats/"); ok {
		r.serveChat(w, req, chat)
		return
	}
	path := strings.TrimPrefix(req.URL.Path, "/v1/handoffs/")
	if path == req.URL.Path {
		http.NotFound(w, req)
		return
	}
	receipt := strings.HasSuffix(path, "/receipt")
	if receipt {
		path = strings.TrimSuffix(path, "/receipt")
	}
	if !code.ValidRoom(path) {
		http.NotFound(w, req)
		return
	}
	switch {
	case req.Method == http.MethodPut && !receipt:
		if !r.admitted(req) {
			r.refuse(w, path, "upload")
			return
		}
		r.put(w, req, path)
	case req.Method == http.MethodGet && !receipt:
		r.claim(w, req, path)
	case req.Method == http.MethodPost && receipt:
		r.ack(w, req, path)
	case req.Method == http.MethodGet && receipt:
		r.status(w, req, path)
	default:
		http.NotFound(w, req)
	}
}

func (r *Relay) put(w http.ResponseWriter, req *http.Request, room string) {
	select {
	case r.uploads <- struct{}{}:
		defer func() { <-r.uploads }()
	default:
		r.log.Warn("upload rejected", "room", tag(room), "reason", "busy")
		http.Error(w, "relay busy", http.StatusServiceUnavailable)
		return
	}
	claim, status := req.Header.Get("X-Handoff-Claim"), req.Header.Get("X-Handoff-Status")
	if !validToken(claim) || !validToken(status) || claim == status {
		r.log.Warn("upload rejected", "room", tag(room), "reason", "malformed tokens")
		http.Error(w, "invalid transfer", http.StatusBadRequest)
		return
	}
	// The deadline covers the whole body, not the gap between reads, so a slow
	// sender cannot keep one of the few upload slots. It is cleared only after
	// a complete read: on failure the server would otherwise try to drain the
	// unread body with no deadline and never send the error response.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(r.config.UploadTimeout))
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, MaxBlob))
	if err == nil {
		rc.SetReadDeadline(time.Time{})
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		r.log.Warn("upload rejected", "room", tag(room), "reason", "upload timed out",
			"bytes", len(body), "timeout", r.config.UploadTimeout)
		w.Header().Set("Connection", "close")
		http.Error(w, "upload timed out", http.StatusRequestTimeout)
		return
	}
	if err != nil || len(body) < 29 {
		r.log.Warn("upload rejected", "room", tag(room), "reason", "invalid or oversized body", "bytes", len(body))
		http.Error(w, "invalid or oversized transfer", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	if r.closed {
		http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, exists := r.rooms[room]; exists {
		r.log.Warn("upload rejected", "room", tag(room), "reason", "code collision")
		http.Error(w, "code collision", http.StatusConflict)
		return
	}
	if len(r.rooms) >= r.config.MaxSessions || r.stored+int64(len(body)) > r.config.MaxStoredBytes {
		active, stored := r.levelsLocked()
		r.log.Warn("upload rejected", "room", tag(room), "reason", "relay full",
			"active", active, "max_sessions", r.config.MaxSessions,
			"stored", stored, "max_stored", r.config.MaxStoredBytes, "wanted", len(body))
		http.Error(w, "relay full", http.StatusServiceUnavailable)
		return
	}
	s := &session{claim: claim, status: status, blob: body, expires: time.Now().Add(r.config.TTL)}
	r.rooms[room] = s
	r.stored += int64(len(body))
	time.AfterFunc(r.config.TTL, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.rooms[room] == s {
			r.stored -= int64(len(s.blob))
			delete(r.rooms, room)
			r.log.Info("session expired", "room", tag(room), "bytes", len(s.blob), "claimed", s.claimed)
		}
	})
	active, stored := r.levelsLocked()
	r.log.Info("stored", "room", tag(room), "bytes", len(body), "ttl", r.config.TTL,
		"active", active, "stored_total", stored)
	w.Header().Set("X-Handoff-Expires-In", strconv.Itoa(int(r.config.TTL/time.Second)))
	w.WriteHeader(http.StatusCreated)
}

func bearer(req *http.Request) string {
	v := req.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(v, "Bearer ")
}

func (r *Relay) claim(w http.ResponseWriter, req *http.Request, room string) {
	token := bearer(req)
	r.mu.Lock()
	r.expireLocked()
	s := r.rooms[room]
	if s == nil || s.claimed || !validToken(token) || !equal(token, s.claim) {
		r.mu.Unlock()
		r.log.Warn("claim denied", "room", tag(room))
		http.NotFound(w, req)
		return
	}
	s.claimed = true
	blob := s.blob
	s.blob = nil
	r.stored -= int64(len(blob))
	active, stored := r.levelsLocked()
	r.mu.Unlock()
	r.log.Info("claimed", "room", tag(room), "bytes", len(blob), "active", active, "stored_total", stored)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", stringLength(len(blob)))
	w.WriteHeader(http.StatusOK)
	w.Write(blob)
}

func (r *Relay) ack(w http.ResponseWriter, req *http.Request, room string) {
	token := bearer(req)
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, 128))
	if err != nil || !validToken(string(body)) {
		http.Error(w, "invalid receipt", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	s := r.rooms[room]
	if s == nil || !s.claimed || s.receipt != "" || !validToken(token) || !equal(token, s.claim) {
		http.NotFound(w, req)
		return
	}
	// The relay cannot verify the end-to-end MAC. The sender does that.
	s.receipt = string(body)
	r.log.Info("receipt posted", "room", tag(room))
	w.WriteHeader(http.StatusNoContent)
}

func (r *Relay) status(w http.ResponseWriter, req *http.Request, room string) {
	token := bearer(req)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	s := r.rooms[room]
	if s == nil || !validToken(token) || !equal(token, s.status) {
		http.NotFound(w, req)
		return
	}
	if s.receipt == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(s.receipt))
	delete(r.rooms, room)
	active, stored := r.levelsLocked()
	r.log.Info("session complete", "room", tag(room), "active", active, "stored_total", stored)
}

func stringLength(n int) string {
	// strconv.Itoa is kept here to keep response construction explicit.
	return strconv.Itoa(n)
}

const linkPage = `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>jand 接收链接</title>
<style>
body{margin:0;font:16px/1.7 -apple-system,BlinkMacSystemFont,"PingFang SC","Segoe UI","Noto Sans CJK SC",sans-serif;background:#f5f6f8;color:#1d2330}
main{max-width:640px;margin:0 auto;padding:32px 16px}
.card{background:#fff;border:1px solid #e2e5eb;border-radius:10px;padding:16px 20px;margin:16px 0}
code{background:#eef0f3;border-radius:4px;padding:1px 5px;font-size:14px;word-break:break-all}
@media (prefers-color-scheme:dark){body{background:#14171c;color:#e6e8ec}.card{background:#1c2027;border-color:#2c323c}code{background:#232831}}
</style></head><body><main>
<h1>这是一个 jand 接收链接</h1>
<p>有人通过 jand 给你发来了一个任务交接。内容是端到端加密的，这个页面看不到，也不会记录链接里 # 后面的部分。</p>
<div class="card"><b>已经装了 jand</b><br>把浏览器地址栏里的<b>完整链接</b>发给你的 AI Agent，说「用 jand 收一下」。它会先把内容讲给你听，你同意了才动手。</div>
<div class="card"><b>还没装</b><br>对你的 Agent 说：「帮我安装 jand，然后用它收下这个链接」，再把完整链接发给它。<br>
自己装：macOS / Linux 运行 <code>curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | sh</code>，Windows 运行 <code>irm https://raw.githubusercontent.com/jaredchao/jand/main/install.ps1 | iex</code>。</div>
<p>链接只能用一次，并且有有效期（默认 30 分钟）。过期了请让对方重新发。</p>
</main></body></html>
`
