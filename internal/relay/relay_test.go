package relay

import (
	"bytes"
	"net/http"
	"net/http/httptest"
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
