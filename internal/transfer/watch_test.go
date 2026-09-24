package transfer

import (
	"context"
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
