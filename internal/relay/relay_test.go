package relay

import (
	"bufio"
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/code"
)

func put(t *testing.T, client *http.Client, base string, c code.Code, blob []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, base+"/v1/handoffs/"+c.Room(), bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Handoff-Claim", c.Token("claim"))
	req.Header.Set("X-Handoff-Status", c.Token("status"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestCapacityCollisionAndExpiry(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSessions = 1
	cfg.MaxStoredBytes = 100
	cfg.TTL = 40 * time.Millisecond
	r := New(cfg)
	defer r.Close()
	srv := httptest.NewServer(r)
	defer srv.Close()
	c, _ := code.New()
	if got := put(t, srv.Client(), srv.URL, c, bytes.Repeat([]byte("x"), 101)); got != http.StatusServiceUnavailable {
		t.Fatalf("over budget: %d", got)
	}
	if got := put(t, srv.Client(), srv.URL, c, bytes.Repeat([]byte("x"), 32)); got != http.StatusCreated {
		t.Fatalf("valid upload: %d", got)
	}
	if got := put(t, srv.Client(), srv.URL, c, bytes.Repeat([]byte("x"), 32)); got != http.StatusConflict {
		t.Fatalf("collision: %d", got)
	}
	other, _ := code.New()
	if got := put(t, srv.Client(), srv.URL, other, bytes.Repeat([]byte("x"), 32)); got != http.StatusServiceUnavailable {
		t.Fatalf("session limit: %d", got)
	}
	time.Sleep(80 * time.Millisecond)
	if r.Active() != 0 {
		t.Fatal("expired ciphertext retained")
	}
	if got := put(t, srv.Client(), srv.URL, other, bytes.Repeat([]byte("x"), 32)); got != http.StatusCreated {
		t.Fatalf("capacity not released: %d", got)
	}
}

func TestTimerExpiryIsLogged(t *testing.T) {
	var logs bytes.Buffer
	cfg := DefaultConfig()
	cfg.TTL = 40 * time.Millisecond
	cfg.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	r := New(cfg)
	defer r.Close()
	srv := httptest.NewServer(r)
	defer srv.Close()
	c, _ := code.New()
	if got := put(t, srv.Client(), srv.URL, c, bytes.Repeat([]byte("x"), 32)); got != http.StatusCreated {
		t.Fatalf("valid upload: %d", got)
	}
	// No request arrives after the upload, so only the TTL timer can expire it.
	time.Sleep(120 * time.Millisecond)
	r.mu.Lock()
	out := logs.String()
	r.mu.Unlock()
	if !strings.Contains(out, `msg="session expired" room=`+c.Room()[:8]) {
		t.Fatalf("expiry not logged:\n%s", out)
	}
}

func TestSlowUploadReleasesSlot(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxSessions = 1 // one upload slot
	cfg.UploadTimeout = 150 * time.Millisecond
	r := New(cfg)
	defer r.Close()
	srv := httptest.NewServer(r)
	defer srv.Close()
	slow, _ := code.New()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT /v1/handoffs/%s HTTP/1.1\r\nHost: relay\r\nX-Handoff-Claim: %s\r\nX-Handoff-Status: %s\r\nContent-Length: 1000\r\n\r\npartial",
		slow.Room(), slow.Token("claim"), slow.Token("status"))
	time.Sleep(50 * time.Millisecond)
	other, _ := code.New()
	if got := put(t, srv.Client(), srv.URL, other, bytes.Repeat([]byte("x"), 32)); got != http.StatusServiceUnavailable {
		t.Fatalf("slot not held by slow upload: %d", got)
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	status, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.Contains(status, "408") {
		t.Fatalf("slow upload not cut off: %q %v", status, err)
	}
	if got := put(t, srv.Client(), srv.URL, other, bytes.Repeat([]byte("x"), 32)); got != http.StatusCreated {
		t.Fatalf("slot not released: %d", got)
	}
}
