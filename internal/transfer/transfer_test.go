package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/code"
	"github.com/jaredchao/jand/internal/relay"
)

func fixture(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "任务.md")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func server(t *testing.T, cfg relay.Config) (*relay.Relay, *httptest.Server) {
	t.Helper()
	r := relay.New(cfg)
	s := httptest.NewServer(r)
	t.Cleanup(s.Close)
	t.Cleanup(r.Close)
	return r, s
}

func TestOfflineSingleUseTransfer(t *testing.T) {
	for _, size := range []int{0, 1, 65543, maxFile} {
		t.Run(stringSize(size), func(t *testing.T) {
			r, s := server(t, relay.DefaultConfig())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			content := bytes.Repeat([]byte("z"), size)
			var invite string
			sendOptions := Options{RelayURL: s.URL, Emit: func(e Event) {
				if e.Event == "queued" {
					invite = e.Code
					if e.Relay != s.URL {
						t.Errorf("queued relay %q, want %q", e.Relay, s.URL)
					}
				}
			}}
			if err := Send(ctx, fixture(t, content), sendOptions); err != nil {
				t.Fatal(err)
			}
			if invite == "" || r.Active() != 1 {
				t.Fatal("ciphertext was not queued")
			}
			out := t.TempDir()
			path, err := Receive(ctx, invite, Options{RelayURL: s.URL, OutputDir: out})
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, content) {
				t.Fatal("content mismatch", err)
			}
			if _, err := Receive(ctx, invite, Options{RelayURL: s.URL, OutputDir: out}); err == nil {
				t.Fatal("used code was accepted")
			}
		})
	}
}

func stringSize(n int) string { return strconv.Itoa(n) }

func TestWaitForAuthenticatedReceipt(t *testing.T) {
	_, s := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queued := make(chan string, 1)
	delivered := make(chan bool, 1)
	done := make(chan error, 1)
	source := fixture(t, []byte("handoff"))
	go func() {
		done <- Send(ctx, source, Options{RelayURL: s.URL, WaitTimeout: 3 * time.Second, Emit: func(e Event) {
			if e.Event == "queued" {
				queued <- e.Code
			}
			if e.Event == "delivered" {
				delivered <- true
			}
		}})
	}()
	invite := <-queued
	if _, err := Receive(ctx, invite, Options{RelayURL: s.URL, OutputDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	default:
		t.Fatal("missing verified delivery event")
	}
}

func TestWrongCodeDoesNotConsumeAndExpiry(t *testing.T) {
	cfg := relay.DefaultConfig()
	cfg.TTL = 80 * time.Millisecond
	r, s := server(t, cfg)
	ctx := context.Background()
	var invite string
	err := Send(ctx, fixture(t, []byte("private")), Options{RelayURL: s.URL, Emit: func(e Event) { invite = e.Code }})
	if err != nil {
		t.Fatal(err)
	}
	other, _ := code.New()
	if _, err := Receive(ctx, other.String(), Options{RelayURL: s.URL, OutputDir: t.TempDir()}); err == nil {
		t.Fatal("unrelated code worked")
	}
	if r.Active() != 1 {
		t.Fatal("wrong code consumed session")
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := Receive(ctx, invite, Options{RelayURL: s.URL, OutputDir: t.TempDir()}); err == nil {
		t.Fatal("expired code worked")
	}
	if r.Active() != 0 {
		t.Fatal("expired session retained")
	}
}

func TestCiphertextTamperAndOneClaim(t *testing.T) {
	_, s := server(t, relay.DefaultConfig())
	c, _ := code.New()
	blob, _, err := encrypt(c, "file.md", []byte("secret"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	blob[len(blob)-1] ^= 1
	target, _ := endpoint(s.URL, c.Room(), false)
	req, _ := http.NewRequest(http.MethodPut, target, bytes.NewReader(blob))
	req.Header.Set("X-Handoff-Claim", c.Token("claim"))
	req.Header.Set("X-Handoff-Status", c.Token("status"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatal("setup failed", err)
	}
	resp.Body.Close()
	out := t.TempDir()
	if _, err := Receive(context.Background(), c.String(), Options{RelayURL: s.URL, OutputDir: out}); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	if _, err := Receive(context.Background(), c.String(), Options{RelayURL: s.URL, OutputDir: out}); err == nil {
		t.Fatal("failed claim could be replayed")
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 0 {
		t.Fatal("tamper left a file")
	}
}

func TestRelayBlobContainsNoPrivateFields(t *testing.T) {
	c, _ := code.New()
	content := []byte("PRIVATE_HANDOFF_CONTEXT_8375829")
	blob, _, err := encrypt(c, "private-task.md", content, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range [][]byte{content, []byte("private-task.md"), []byte(c.String())} {
		if bytes.Contains(blob, private) {
			t.Fatal("plaintext visible in relay blob")
		}
	}
	wrong, _ := code.New()
	if _, _, err := decrypt(wrong, blob); err == nil {
		t.Fatal("wrong secret decrypted blob")
	}
	blob[len(blob)-1] ^= 1
	if _, _, err := decrypt(c, blob); err == nil {
		t.Fatal("modified blob decrypted")
	}
}

func TestReceiptFailurePreservesSavedFile(t *testing.T) {
	r := relay.New(relay.DefaultConfig())
	defer r.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost {
			http.Error(w, "dropped", 503)
			return
		}
		r.ServeHTTP(w, req)
	}))
	defer s.Close()
	var invite string
	if err := Send(context.Background(), fixture(t, []byte("saved")), Options{RelayURL: s.URL, Emit: func(e Event) { invite = e.Code }}); err != nil {
		t.Fatal(err)
	}
	path, err := Receive(context.Background(), invite, Options{RelayURL: s.URL, OutputDir: t.TempDir()})
	if !errors.Is(err, ErrUnconfirmed) || path == "" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	if b, readErr := os.ReadFile(path); readErr != nil || string(b) != "saved" {
		t.Fatal("saved file missing", readErr)
	}
}

func TestForgedReceiptCannotBecomeDelivery(t *testing.T) {
	r := relay.New(relay.DefaultConfig())
	defer r.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && len(req.URL.Path) > 8 && req.URL.Path[len(req.URL.Path)-8:] == "/receipt" {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("forged"))
			return
		}
		r.ServeHTTP(w, req)
	}))
	defer s.Close()
	source := fixture(t, []byte("not yet saved"))
	var queued bool
	err := Send(context.Background(), source, Options{RelayURL: s.URL, WaitTimeout: time.Second, Emit: func(e Event) {
		if e.Event == "queued" {
			queued = true
		}
		if e.Event == "delivered" {
			t.Error("forged receipt reported delivered")
		}
	}})
	if !queued || !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("queued=%v err=%v", queued, err)
	}
}

func TestUnsafeReceivedFilename(t *testing.T) {
	for _, name := range []string{"../escape", "a/b", "a\\b", "CON.txt", "name.", ""} {
		if _, err := newSink(t.TempDir(), name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestBounds(t *testing.T) {
	_, s := server(t, relay.DefaultConfig())
	f, err := os.CreateTemp(t.TempDir(), "large")
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(maxFile + 1)
	f.Close()
	if err := Send(context.Background(), f.Name(), Options{RelayURL: s.URL}); err == nil {
		t.Fatal("oversized file accepted")
	}
	if _, err := endpoint("ws://example.com", "abc", false); err == nil {
		t.Fatal("websocket URL accepted")
	}
}

func TestRejectionCarriesRelayReason(t *testing.T) {
	cfg := relay.DefaultConfig()
	cfg.MaxSessions = 1
	_, s := server(t, cfg)
	ctx := context.Background()
	if err := Send(ctx, fixture(t, []byte("first")), Options{RelayURL: s.URL}); err != nil {
		t.Fatal(err)
	}
	err := Send(ctx, fixture(t, []byte("second")), Options{RelayURL: s.URL})
	if err == nil || err.Error() != "relay rejected transfer: HTTP 503: relay full" {
		t.Fatalf("err=%v", err)
	}
	if got := relayReason([]byte("busy\x1b[31m\r\nnow")); got != "busy [31m now" {
		t.Fatalf("control characters kept: %q", got)
	}
}

func TestInterruptedDownloadSaysCodeIsUsed(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("short"))
	}))
	defer s.Close()
	c, _ := code.New()
	_, err := Receive(context.Background(), c.String(), Options{RelayURL: s.URL, OutputDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "ask the sender to send again") {
		t.Fatalf("err=%v", err)
	}
}

func TestAccessTokenGuardsCreationOnly(t *testing.T) {
	token := "team-secret"
	sum := sha256.Sum256([]byte(token))
	cfg := relay.DefaultConfig()
	cfg.AccessTokenHashes = [][]byte{sum[:]}
	_, s := server(t, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, bad := range []string{"", "wrong"} {
		err := Send(ctx, fixture(t, []byte("x")), Options{RelayURL: s.URL, AccessToken: bad})
		if err == nil || !strings.Contains(err.Error(), "HTTP 401") || !strings.Contains(err.Error(), "access token") {
			t.Fatalf("token %q: %v", bad, err)
		}
		err = Send(ctx, fixture(t, []byte("x")), Options{RelayURL: s.URL, AccessToken: bad, Chat: true, Goal: "g", StateDir: t.TempDir()})
		if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
			t.Fatalf("chat, token %q: %v", bad, err)
		}
	}
	var invite string
	o := Options{RelayURL: s.URL, AccessToken: token, Emit: func(e Event) {
		if e.Event == "queued" {
			invite = e.Code
		}
	}}
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
	// The receiver has only the code, no token.
	if _, err := Receive(ctx, invite, Options{RelayURL: s.URL, OutputDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	o.Chat, o.Goal, o.StateDir = true, "g", t.TempDir()
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
}
