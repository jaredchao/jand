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
		// code after # never reaches the server; inline JS only reads
		// window.location.href locally to help the user copy a prompt.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-"+base64.StdEncoding.EncodeToString(linkPageScriptHash[:])+"'")
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

var linkPageScriptHash = sha256.Sum256([]byte(linkPageScript))

const linkPage = `<!doctype html>
<html lang="zh-Hans"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>jand 接收链接 · 让两边的 AI 直接对齐</title>
<style>
:root{
  --bg:#fafafa;--surface:#ffffff;--subtle:#f4f4f5;--ink:#09090b;--muted:#63636e;
  --line:rgba(9,9,11,.09);--line-strong:rgba(9,9,11,.16);
  --accent:#0d9488;--accent-bright:#14b8a6;--accent-ink:#0f766e;--accent-soft:rgba(13,148,136,.09);
  --amber:#c2410c;--amber-soft:rgba(234,88,12,.09);--code-bg:#f4f4f5;
  --grid:rgba(9,9,11,.045);--inner:inset 0 1px 0 rgba(255,255,255,.85);
  --shadow:0 24px 60px -12px rgba(9,9,11,.09),0 4px 14px -2px rgba(9,9,11,.04);
}
@media (prefers-color-scheme:dark){:root{
  --bg:#09090b;--surface:#111114;--subtle:#18181b;--ink:#fafafa;--muted:#9898a6;
  --line:rgba(255,255,255,.09);--line-strong:rgba(255,255,255,.17);
  --accent:#2dd4bf;--accent-bright:#5eead4;--accent-ink:#5eead4;--accent-soft:rgba(45,212,191,.11);
  --amber:#fb923c;--amber-soft:rgba(251,146,60,.12);--code-bg:#16161a;
  --grid:rgba(255,255,255,.045);--inner:inset 0 1px 0 rgba(255,255,255,.06);
  --shadow:0 28px 70px -12px rgba(0,0,0,.65),0 0 0 1px rgba(255,255,255,.05);
}}
*{box-sizing:border-box}
body{
  margin:0;min-height:100vh;color:var(--ink);background:var(--bg);
  font:15px/1.68 -apple-system,BlinkMacSystemFont,"Inter","PingFang SC","Hiragino Sans GB","Segoe UI","Microsoft YaHei",sans-serif;
  -webkit-font-smoothing:antialiased;
}
body::before{
  content:"";position:fixed;inset:0;z-index:-1;pointer-events:none;
  background-image:linear-gradient(to right,var(--grid) 1px,transparent 1px),linear-gradient(to bottom,var(--grid) 1px,transparent 1px);
  background-size:44px 44px;
  -webkit-mask-image:radial-gradient(ellipse 80% 55% at 50% 0%,#000 35%,transparent 100%);
  mask-image:radial-gradient(ellipse 80% 55% at 50% 0%,#000 35%,transparent 100%);
}
main{max-width:700px;margin:0 auto;padding:40px 18px 56px}
.top{display:flex;align-items:center;justify-content:space-between;margin-bottom:24px}
.brand{display:inline-flex;align-items:center;gap:10px;text-decoration:none;color:var(--ink);font-weight:800;font-size:18px;letter-spacing:-.02em}
.logo{width:26px;height:26px}
.logo path{fill:none;stroke-width:13;stroke-linecap:square}
.logo-ink{stroke:var(--ink)}.logo-accent{stroke:var(--accent)}
.badge{
  display:inline-flex;align-items:center;gap:7px;font-size:12px;font-weight:650;
  color:var(--accent-ink);background:var(--surface);border:1px solid var(--line-strong);
  padding:5px 12px;border-radius:999px;
}
.badge::before{content:"";width:7px;height:7px;border-radius:50%;background:var(--accent)}
.hero{background:var(--surface);border:1px solid var(--line-strong);border-radius:20px;padding:28px;box-shadow:var(--shadow),var(--inner)}
h1{font-size:clamp(22px,4vw,28px);line-height:1.2;letter-spacing:-.03em;margin:0 0 8px}
.lead{margin:0 0 20px;color:var(--muted);font-size:14.5px}
.notice{display:none;background:var(--amber-soft);color:var(--amber);border:1px solid color-mix(in srgb,var(--amber) 25%,transparent);border-radius:10px;padding:10px 14px;font-size:13px;font-weight:600;margin-bottom:16px}
.tabs{display:inline-flex;flex-wrap:wrap;gap:4px;padding:4px;border-radius:11px;background:var(--subtle);border:1px solid var(--line);margin-bottom:14px}
.tab{
  appearance:none;border:none;background:transparent;color:var(--muted);
  padding:7px 13px;border-radius:8px;font-family:inherit;font-size:12.5px;font-weight:600;line-height:1.2;cursor:pointer;transition:.15s;
}
.tab:hover{color:var(--ink)}
.tab.active{background:var(--surface);color:var(--ink);box-shadow:0 1px 4px rgba(0,0,0,.1)}
.prompt-box{
  background:var(--code-bg);border:1px solid var(--line-strong);border-radius:12px;
  padding:14px 16px;font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  font-size:13px;line-height:1.68;word-break:break-all;margin-bottom:16px;white-space:pre-wrap;
}
.actions{display:flex;flex-wrap:wrap;gap:10px;align-items:center}
.btn{
  appearance:none;border:1px solid transparent;border-radius:10px;padding:11px 20px;
  font-family:inherit;font-size:14px;font-weight:650;line-height:1.2;cursor:pointer;transition:transform .12s,opacity .15s,border-color .15s;
  display:inline-flex;align-items:center;gap:8px;
}
.btn:active{transform:scale(.98)}
.btn-primary{background:linear-gradient(180deg,var(--accent-bright),var(--accent));color:#fff;box-shadow:0 8px 20px -6px rgba(13,148,136,.4)}
@media (prefers-color-scheme:dark){.btn-primary{color:#042f2e;font-weight:750}}
.btn-secondary{background:var(--surface);color:var(--ink);border-color:var(--line-strong)}
.btn-secondary:hover,.btn-primary:hover{opacity:.93}
.btn:disabled{opacity:.5;cursor:default}
.copy-status{margin:8px 0 0;color:var(--muted);font-size:13px}
.copy-status:empty{display:none}
.trust{display:grid;grid-template-columns:repeat(3,1fr);gap:10px;margin-top:22px;padding-top:20px;border-top:1px solid var(--line)}
.trust-item{background:var(--subtle);border:1px solid var(--line);border-radius:12px;padding:12px 14px}
.trust-item b{display:block;font-size:13px;margin-bottom:3px;color:var(--ink)}
.trust-item span{display:block;font-size:12px;color:var(--muted);line-height:1.5}
.more{margin-top:16px;background:var(--surface);border:1px solid var(--line);border-radius:16px;padding:15px 20px}
details summary{cursor:pointer;font-weight:650;font-size:14px;color:var(--ink);list-style:none;display:flex;align-items:center;justify-content:space-between}
details summary::-webkit-details-marker{display:none}
details summary::after{content:"展开 ▾";font-size:12px;font-weight:600;color:var(--muted)}
details[open] summary::after{content:"收起 ▴"}
.more-body{margin-top:14px;padding-top:14px;border-top:1px solid var(--line);font-size:13.5px;color:var(--muted)}
.more-body p{margin:0 0 10px}
pre{margin:0 0 12px;background:var(--code-bg);border:1px solid var(--line);border-radius:10px;padding:11px 13px;overflow-x:auto;color:var(--ink)}
code{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px}
.more-body a{color:var(--accent-ink);font-weight:600}
footer{margin-top:22px;display:flex;justify-content:space-between;flex-wrap:wrap;gap:10px;font-size:12.5px;color:var(--muted)}
footer a{color:var(--muted);text-decoration:none}
footer a:hover{color:var(--ink)}
@media (max-width:600px){
  main{padding:22px 14px 40px}
  .hero{padding:20px}
  .trust{grid-template-columns:1fr}
  .btn{width:100%;justify-content:center}
  .tabs{display:flex;width:100%}
  .tab{flex:1;text-align:center}
}
</style></head><body><main>
<header class="top">
  <a class="brand" href="https://github.com/jaredchao/jand" target="_blank" rel="noopener">
    <svg class="logo" viewBox="0 0 160 160" aria-hidden="true"><path d="M56 28H39C30 28 23 35 23 44V116C23 125 30 132 39 132H56M104 28H121C130 28 137 35 137 44V116C137 125 130 132 121 132H104" class="logo-ink"/><path d="M60 80H93M84 62L103 80L84 98" class="logo-accent"/></svg>
    <span>jand</span>
  </a>
  <span class="badge">端到端加密 · 一次有效</span>
</header>

<section class="hero">
  <h1>收到一份 AI 协作邀请</h1>
  <p class="lead">有人通过 jand 给你发来了一个任务交接或协作邀请。点击下方按钮复制指令，直接粘贴给你的 AI Agent 即可。</p>
  <div id="no-code" class="notice">提示：当前地址栏没有检测到 # 后面的接收码，请确认你打开的是对方发来的完整链接。</div>

  <div class="tabs" role="group" aria-label="选择你的 AI 工具">
    <button class="tab active" type="button" aria-pressed="true" data-mode="general">Claude Code / Codex / 通用 AI</button>
    <button class="tab" type="button" aria-pressed="false" data-mode="workbuddy">WorkBuddy 技能</button>
    <button class="tab" type="button" aria-pressed="false" data-mode="dsh">DSH 插件</button>
  </div>

  <div id="prompt-box" class="prompt-box">把地址栏里的完整链接发给你的 AI，说「用 jand 收一下，先告诉我内容与目标，等我确认后再继续」。</div>

  <div class="actions">
    <button id="copy-prompt" class="btn btn-primary" type="button" disabled>一键复制，粘贴给你的 AI</button>
    <button id="copy-link" class="btn btn-secondary" type="button" disabled>仅复制完整链接</button>
  </div>
  <p id="copy-status" class="copy-status" role="status"></p>

  <div class="trust">
    <div class="trust-item"><b>不会自动干活</b><span>你的 AI 只会先解包读给你听，你明确同意前什么都不会改。</span></div>
    <div class="trust-item"><b>本机隐私不出门</b><span>内容端到端加密，中继与本页都看不到，# 后的接收码不会发往服务器。</span></div>
    <div class="trust-item"><b>链接一次有效</b><span>默认有效期内仅可领取一次，不满意随时拒绝或关闭。</span></div>
  </div>
</section>

<section class="more">
  <details>
    <summary>还没装 jand？手动安装与国内直链下载</summary>
    <div class="more-body">
      <p>大多数情况下直接把上面的指令发给你的 AI，它会自动帮你安装。也可以自己装：</p>
      <p><strong>macOS / Linux</strong></p>
      <pre><code>curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | sh</code></pre>
      <p><strong>Windows（PowerShell）</strong></p>
      <pre><code>irm https://raw.githubusercontent.com/jaredchao/jand/main/install.ps1 | iex</code></pre>
      <p>访问 GitHub 较慢时，可直接从本站下载各平台预编译包或 <code>dsh-jand</code> 插件清单：<a href="https://jand.zhou-wei.com/downloads/index.json" target="_blank" rel="noopener">downloads/index.json</a>。</p>
    </div>
  </details>
</section>

<footer>
  <span>jand 接收链接 · 人始终是授权方</span>
  <span><a href="https://github.com/jaredchao/jand" target="_blank" rel="noopener">GitHub 开源项目</a></span>
</footer>
</main>
<script>` + linkPageScript + `</script>
</body></html>
`

const linkPageScript = `
(function(){
  var href = window.location.href;
  var hasCode = window.location.hash && window.location.hash.length > 1;
  var linkText = hasCode ? href : '<请替换为对方发来的完整 jand 接收链接>';
  if (!hasCode) {
    document.getElementById('no-code').style.display = 'block';
  }
  var templates = {
    general: '请帮我用 jand 接收这个协作邀请（如果本机还没装 jand，先帮我安装；解包后先给我看内容和目标，我同意后再决定是否加入）：\n' + linkText,
    workbuddy: '用 jand 技能帮我接收这个链接，先告诉我对方发了什么、目标是什么，等我确认后再决定要不要加入：\n' + linkText,
    dsh: '用 dsh-jand 插件接收这个协作链接，先把摘要和目标展示给我确认：\n' + linkText
  };
  var mode = 'general';
  var box = document.getElementById('prompt-box');
  var copyPromptBtn = document.getElementById('copy-prompt');
  var copyLinkBtn = document.getElementById('copy-link');
  var status = document.getElementById('copy-status');
  copyPromptBtn.disabled = copyLinkBtn.disabled = !hasCode;

  function render() {
    box.textContent = templates[mode];
  }
  async function copyText(text, btn, doneLabel, failureLabel) {
    status.textContent = '';
    try {
      await navigator.clipboard.writeText(text);
      status.textContent = doneLabel;
    } catch (error) {
      status.textContent = fallback(text, btn) ? doneLabel : failureLabel;
    }
  }
  function fallback(text, btn) {
    var ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try {
      return document.execCommand('copy');
    } catch (error) {
      return false;
    } finally {
      ta.remove();
      btn.focus();
    }
  }
  var tabs = document.querySelectorAll('.tab');
  for (var i = 0; i < tabs.length; i++) {
    tabs[i].addEventListener('click', function(){
      for (var j = 0; j < tabs.length; j++) {
        var active = tabs[j] === this;
        tabs[j].classList.toggle('active', active);
        tabs[j].setAttribute('aria-pressed', String(active));
      }
      mode = this.getAttribute('data-mode');
      status.textContent = '';
      render();
    });
  }
  copyPromptBtn.addEventListener('click', function(){
    copyText(templates[mode], copyPromptBtn, '已复制指令，可以粘贴给你的 AI。', '复制失败，请手动选中并复制上方指令。');
  });
  copyLinkBtn.addEventListener('click', function(){
    copyText(linkText, copyLinkBtn, '已复制完整链接。', '复制失败，请手动复制地址栏中的完整链接。');
  });
  render();
})();
`
