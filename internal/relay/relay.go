// Package relay holds bounded, expiring ciphertext in memory. It never
// receives a transfer code or a file encryption key.
package relay

import (
	"crypto/subtle"
	"encoding/base64"
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
}

func DefaultConfig() Config {
	return Config{MaxSessions: 128, MaxStoredBytes: 64 * 1024 * 1024, TTL: 10 * time.Minute, UploadTimeout: 2 * time.Minute}
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
	closed  bool
	uploads chan struct{}
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
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Relay{config: config, log: logger, started: time.Now(),
		rooms: make(map[string]*session), uploads: make(chan struct{}, min(8, config.MaxSessions))}
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
	if req.URL.Path == "/healthz" && req.Method == http.MethodGet {
		// Body added so an operator can tell a live process from a stale proxy
		// cache or a listener that accepts but no longer serves. Session counts
		// are deliberately omitted: this endpoint is unauthenticated.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"status":"ok","uptime_seconds":%d}`+"\n", int64(time.Since(r.started).Seconds()))
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
