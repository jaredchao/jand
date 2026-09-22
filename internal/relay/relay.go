// Package relay holds bounded, expiring ciphertext in memory. It never
// receives a transfer code or a file encryption key.
package relay

import (
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
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
}

func DefaultConfig() Config {
	return Config{MaxSessions: 128, MaxStoredBytes: 64 * 1024 * 1024, TTL: 10 * time.Minute}
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
	return &Relay{config: config, rooms: make(map[string]*session), uploads: make(chan struct{}, min(8, config.MaxSessions))}
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
		w.WriteHeader(http.StatusOK)
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
		http.Error(w, "relay busy", http.StatusServiceUnavailable)
		return
	}
	claim, status := req.Header.Get("X-Handoff-Claim"), req.Header.Get("X-Handoff-Status")
	if !validToken(claim) || !validToken(status) || claim == status {
		http.Error(w, "invalid transfer", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, MaxBlob))
	if err != nil || len(body) < 29 {
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
		http.Error(w, "code collision", http.StatusConflict)
		return
	}
	if len(r.rooms) >= r.config.MaxSessions || r.stored+int64(len(body)) > r.config.MaxStoredBytes {
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
		}
	})
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
		http.NotFound(w, req)
		return
	}
	s.claimed = true
	blob := s.blob
	s.blob = nil
	r.stored -= int64(len(blob))
	r.mu.Unlock()
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
}

func stringLength(n int) string {
	// strconv.Itoa is kept here to keep response construction explicit.
	return strconv.Itoa(n)
}
