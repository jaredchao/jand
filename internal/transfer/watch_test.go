package transfer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/relay"
)

func TestWatchOnlyLooks(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0) // opened, joined

	seen := make(chan Event, 16)
	watchOpts := host.o
	watchOpts.Emit = func(e Event) { seen <- e }
	done := make(chan error, 1)
	go func() { done <- ChatWatch(ctx, id, 50*time.Millisecond, watchOpts) }()

	if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "字段？"}, guest.o); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-seen:
		if e.Event != "message" || e.ID != "g1" || e.Kind != "request" || e.Text != "字段？" || !e.Unread || !e.Untrusted {
			t.Fatalf("watch saw %+v", e)
		}
	case <-ctx.Done():
		t.Fatal("watch saw nothing")
	}
	time.Sleep(200 * time.Millisecond) // several more polls: the same message must not repeat
	if len(seen) != 0 {
		t.Fatalf("watch repeated an event: %+v", <-seen)
	}
	// The agent still receives what the watcher saw.
	if ev, err := host.recv(ctx, id, 0); err != nil || types(ev) != "message" || ev[0].ID != "g1" {
		t.Fatalf("agent after watch: %v %v", types(ev), err)
	}
	if err := ChatClose(ctx, id, guest.o); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("watch did not stop when the chat closed")
	}
}

func TestAgentWatchRetriesCoalescesAndLeavesUnread(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)
	for _, kind := range []string{"request", "delivery"} {
		if err := ChatSend(ctx, id, ChatMessage{Kind: kind, Text: "peer content must not become a host prompt"}, guest.o); err != nil {
			t.Fatal(err)
		}
	}
	attempts := make(chan int, 10)
	emitted := make(chan Event, 20)
	o := host.o
	o.Emit = func(e Event) { emitted <- e }
	done := make(chan error, 1)
	watchCtx, stop := context.WithCancel(ctx)
	defer stop()
	n := 0
	go func() {
		done <- ChatWatchNotify(watchCtx, id[:8], 10*time.Millisecond, o, func(_ context.Context, chat string) error {
			n++
			if chat != id {
				return errors.New("not resolved to exact chat")
			}
			attempts <- n
			if n == 1 {
				return errors.New("host unavailable")
			}
			return nil
		})
	}()
	for want := 1; want <= 2; want++ {
		select {
		case got := <-attempts:
			if got != want {
				t.Fatalf("attempt %d, want %d", got, want)
			}
		case <-ctx.Done():
			t.Fatal("no notification/retry")
		}
	}
	select {
	case <-attempts:
		t.Fatal("successful batch notified again")
	case <-time.After(80 * time.Millisecond):
	}
	ev, err := host.recv(ctx, id, 0)
	if err != nil || types(ev) != "message,message" {
		t.Fatalf("watch consumed messages: %v %v", types(ev), err)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel outcome: %v", err)
	}
	close(emitted)
	messages, retries := 0, 0
	for e := range emitted {
		if e.Event == "message" {
			messages++
		}
		if e.Event == "agent_notify_retry" {
			retries++
		}
	}
	if messages != 2 || retries != 1 {
		t.Fatalf("messages=%d retries=%d", messages, retries)
	}
}

func TestAgentWatchProgressAndClosingRetry(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, code := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, code, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)
	if err := ChatSend(ctx, id, ChatMessage{Kind: "progress", Text: "working"}, guest.o); err != nil {
		t.Fatal(err)
	}
	progress := make(chan struct{}, 1)
	o := host.o
	o.Emit = func(e Event) {
		if e.Event == "message" && e.Kind == "progress" {
			progress <- struct{}{}
		}
	}
	attempts := make(chan struct{}, 10)
	done := make(chan error, 1)
	n := 0
	go func() {
		done <- ChatWatchNotify(ctx, id, 10*time.Millisecond, o, func(context.Context, string) error {
			n++
			attempts <- struct{}{}
			if n == 1 {
				return errors.New("queue offline")
			}
			return nil
		})
	}()
	select {
	case <-progress:
	case <-ctx.Done():
		t.Fatal("no progress observed")
	}
	select {
	case <-attempts:
		t.Fatal("progress woke agent")
	case <-time.After(50 * time.Millisecond):
	}
	if err := ChatClose(ctx, id, guest.o); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil || n != 2 {
			t.Fatalf("closing notification was lost: attempts=%d err=%v", n, err)
		}
	case <-ctx.Done():
		t.Fatal("closing retry never completed")
	}
}

func TestAgentWatchRejectsCorruptCursor(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, _ := invite(t, context.Background(), host, guest)
	if err := os.WriteFile(filepath.Join(host.o.StateDir, id+".cursor"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := ChatWatchNotify(context.Background(), id, time.Millisecond, host.o, func(context.Context, string) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("corrupt cursor became usable: %v, called=%v", err, called)
	}
}

func TestAgentWatchRelayFailureOutcomes(t *testing.T) {
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusNotFound, http.StatusUnauthorized} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			r := relay.New(relay.DefaultConfig())
			defer r.Close()
			var fail atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if code := int(fail.Load()); code != 0 && strings.HasSuffix(req.URL.Path, "/events") {
					http.Error(w, "test fault", code)
					return
				}
				r.ServeHTTP(w, req)
			}))
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
			id, code := invite(t, ctx, host, guest)
			if _, err := ChatJoin(ctx, code, guest.o); err != nil {
				t.Fatal(err)
			}
			host.recv(ctx, id, 0)
			if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "hello"}, guest.o); err != nil {
				t.Fatal(err)
			}
			fail.Store(int32(status))
			retries, calls := 0, 0
			o := host.o
			o.Emit = func(e Event) {
				if e.Event == "watch_retry" {
					retries++
					fail.Store(0) // Relay recovers; pending events must still be fetched.
				}
			}
			err := ChatWatchNotify(ctx, id, time.Millisecond, o, func(context.Context, string) error {
				calls++
				cancel()
				return nil
			})
			if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
				if retries != 1 || calls != 1 || !errors.Is(err, context.Canceled) {
					t.Fatalf("did not recover: retries=%d calls=%d err=%v", retries, calls, err)
				}
			} else if err == nil || retries != 0 || calls != 0 {
				t.Fatalf("permanent failure became success/retry: %v retries=%d calls=%d", err, retries, calls)
			}
		})
	}
}

func TestWatchStopsAfterAgentAlreadyReadClose(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, code := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, code, guest.o); err != nil {
		t.Fatal(err)
	}
	if err := ChatClose(ctx, id, guest.o); err != nil {
		t.Fatal(err)
	}
	if _, err := host.recv(ctx, id, 0); err != nil {
		t.Fatal(err)
	}
	if err := ChatWatchNotify(ctx, id, time.Millisecond, host.o, func(context.Context, string) error {
		t.Error("already read close notified again")
		return nil
	}); err != nil {
		t.Fatalf("watch did not finish: %v", err)
	}
}
