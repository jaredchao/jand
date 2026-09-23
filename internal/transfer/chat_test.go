package transfer

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"net/http/httputil"
	"net/url"

	"github.com/jaredchao/jand/internal/code"
	"github.com/jaredchao/jand/internal/relay"
)

type side struct {
	t      *testing.T
	o      Options
	events []Event
}

func newSide(t *testing.T, url string) *side {
	s := &side{t: t}
	s.o = Options{RelayURL: url, OutputDir: t.TempDir(), StateDir: t.TempDir(), Emit: func(e Event) { s.events = append(s.events, e) }}
	return s
}

// recv returns the events one ChatRecv call emitted.
func (s *side) recv(ctx context.Context, id string, wait time.Duration) ([]Event, error) {
	s.events = nil
	err := ChatRecv(ctx, id, wait, s.o)
	return s.events, err
}

func types(events []Event) string {
	var out []string
	for _, e := range events {
		out = append(out, e.Event)
	}
	return strings.Join(out, ",")
}

// invite sends a --chat packet from host and has guest claim it.
func invite(t *testing.T, ctx context.Context, host, guest *side) (chatID, rawCode string) {
	t.Helper()
	if err := Send(ctx, fixture(t, []byte("# 交接\n联调接口")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	queued := host.events[0]
	if queued.Event != "queued" || len(queued.Chat) != 32 {
		t.Fatalf("queued: %+v", queued)
	}
	if _, err := Receive(ctx, queued.Code, guest.o); err != nil {
		t.Fatal(err)
	}
	saved := guest.events[len(guest.events)-1]
	if saved.Event != "saved" || !saved.ChatInvite || saved.Chat != "" || saved.Goal != testGoal || !saved.RequiresUserApproval {
		t.Fatalf("saved: %+v", saved)
	}
	return queued.Chat, queued.Code
}

const testGoal = "双方对 /users 响应字段达成一致"

func withChat(o Options) Options {
	o.Chat, o.Goal = true, testGoal
	return o
}

func TestChatConversation(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)

	if ev, err := host.recv(ctx, id[:8], 0); err != nil || types(ev) != "opened" {
		t.Fatalf("host after claim: %v %v", types(ev), err)
	}
	if err := ChatSend(ctx, id, "too early", host.o); err == nil || !strings.Contains(err.Error(), "peer has not joined") {
		t.Fatalf("send before join: %v", err)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	if _, err := ChatJoin(ctx, rawCode, newSide(t, srv.URL).o); err == nil || !strings.Contains(err.Error(), "already joined") {
		t.Fatalf("second join with the same code: %v", err)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "joined" {
		t.Fatalf("host sees join: %v", types(ev))
	}

	if err := ChatSend(ctx, id, "GET /users 返回什么结构？", host.o); err != nil {
		t.Fatal(err)
	}
	ev, err := guest.recv(ctx, id, time.Second)
	if err != nil || types(ev) != "message" || ev[0].Text != "GET /users 返回什么结构？" || !ev[0].Untrusted || ev[0].From != "host" {
		t.Fatalf("guest message: %+v %v", ev, err)
	}
	// Acknowledged events are not delivered twice.
	if ev, _ := guest.recv(ctx, id, 0); types(ev) != "no_events" {
		t.Fatalf("redelivered: %v", types(ev))
	}

	// A blocked recv wakes as soon as the peer sends.
	quiet := guest.o
	quiet.Emit = nil
	sent := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		sent <- ChatSend(ctx, id, "[{id, name}]", quiet)
	}()
	start := time.Now()
	ev, err = host.recv(ctx, id, 30*time.Second)
	if err != nil || types(ev) != "message" || ev[0].Text != "[{id, name}]" {
		t.Fatalf("host long-poll: %+v %v", ev, err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("long-poll did not wake promptly: %v", time.Since(start))
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}

	if err := ChatClose(ctx, id, guest.o); err != nil {
		t.Fatal(err)
	}
	if ev, _ := host.recv(ctx, id, time.Second); types(ev) != "closed" || ev[0].By != "peer" {
		t.Fatalf("host sees close: %+v", ev)
	}
	if _, err := host.recv(ctx, id, 0); !errors.Is(err, ErrChatEnded) {
		t.Fatalf("recv after end: %v", err)
	}
	if err := ChatSend(ctx, id, "late", host.o); !errors.Is(err, ErrChatEnded) {
		t.Fatalf("send after end: %v", err)
	}
	transcript, err := os.ReadFile(host.o.StateDir + "/" + id + ".transcript.jsonl")
	if err != nil || strings.Count(string(transcript), "\n") != 3 || !strings.Contains(string(transcript), "[{id, name}]") {
		t.Fatalf("transcript: %q %v", transcript, err)
	}
	if info, err := os.Stat(host.o.StateDir + "/" + id + ".json"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
}

func TestChatDecline(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if err := ChatDecline(ctx, rawCode, "今天没空，明天再说", guest.o); err != nil {
		t.Fatal(err)
	}
	ev, err := host.recv(ctx, id, time.Second)
	if err != nil || types(ev) != "opened,declined" || ev[1].Text != "今天没空，明天再说" || !ev[1].Untrusted {
		t.Fatalf("host sees decline: %+v %v", ev, err)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); !errors.Is(err, ErrChatEnded) {
		t.Fatalf("join after decline: %v", err)
	}
}

func TestChatExpiresUnclaimed(t *testing.T) {
	cfg := relay.DefaultConfig()
	cfg.ChatInviteTTL = 200 * time.Millisecond
	_, srv := server(t, cfg)
	ctx := context.Background()
	host := newSide(t, srv.URL)
	if err := Send(ctx, fixture(t, []byte("x")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	ev, err := host.recv(ctx, host.events[0].Chat, 3*time.Second)
	if err != nil || types(ev) != "expired" || ev[0].Reason != "unclaimed" {
		t.Fatalf("expiry: %+v %v", ev, err)
	}
}

func TestChatNeedsCapableRelay(t *testing.T) {
	uploads := 0
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/handoffs/") {
			uploads++
		}
		http.NotFound(w, r)
	}))
	defer old.Close()
	host := newSide(t, old.URL)
	err := Send(context.Background(), fixture(t, []byte("x")), withChat(host.o))
	if err == nil || !strings.Contains(err.Error(), "does not support chat") || uploads != 0 {
		t.Fatalf("old relay: %v uploads=%d", err, uploads)
	}
}

func TestChatRejectsForgedReplayedAndTampered(t *testing.T) {
	c, _ := code.New()
	key := c.Derive("chat/key")
	room := chatRoomID(c)
	s := &chatState{ID: room, Role: "guest"}
	dir := t.TempDir()
	good, _ := sealChat(key, room, "host", "message", 5, "hi")
	raw, _ := base64.StdEncoding.DecodeString(good)
	raw[len(raw)/2] ^= 1
	tampered := base64.StdEncoding.EncodeToString(raw)
	cur := &chatCursor{}
	if e := s.translate(dir, key, cur, relayEvent{Seq: 1, Type: "message", From: "host", Ctr: 5, Data: good}); e.Event != "message" || e.Text != "hi" {
		t.Fatalf("valid: %+v", e)
	}
	cases := map[string]relayEvent{
		"replayed counter": {Seq: 2, Type: "message", From: "host", Ctr: 5, Data: good},
		"own direction":    {Seq: 3, Type: "message", From: "guest", Ctr: 6, Data: good},
		"relabelled ctr":   {Seq: 4, Type: "message", From: "host", Ctr: 9, Data: good},
		"tampered":         {Seq: 5, Type: "message", From: "host", Ctr: 7, Data: tampered},
		"host-only event":  {Seq: 6, Type: "joined"},
	}
	for name, ev := range cases {
		if e := s.translate(dir, key, cur, ev); e.Event != "error" {
			t.Errorf("%s accepted: %+v", name, e)
		}
	}
	if cur.PeerCtr != 5 {
		t.Fatalf("rejected events moved the counter: %d", cur.PeerCtr)
	}
}

func TestChatJoinRefusesOwnRoom(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host := newSide(t, srv.URL)
	if err := Send(ctx, fixture(t, []byte("x")), withChat(host.o)); err != nil {
		t.Fatal(err)
	}
	if _, err := ChatJoin(ctx, host.events[0].Code, host.o); err == nil || !strings.Contains(err.Error(), "JAND_HOME") {
		t.Fatalf("join into own state dir: %v", err)
	}
}

func TestChatNeedsGoal(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	host := newSide(t, srv.URL)
	o := host.o
	o.Chat = true
	if err := Send(context.Background(), fixture(t, []byte("x")), o); err == nil || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("chat without goal: %v", err)
	}
	o.Goal = strings.Repeat("目", MaxChatGoal)
	if err := Send(context.Background(), fixture(t, []byte("x")), o); err == nil || !strings.Contains(err.Error(), "goal") {
		t.Fatalf("oversized goal: %v", err)
	}
}

func TestChatJoinRetryAndChatIDMistake(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)
	if _, err := ChatJoin(ctx, id, guest.o); err == nil || !strings.Contains(err.Error(), "not the chat id") {
		t.Fatalf("join with chat id: %v", err)
	}
	// A gateway that forwards the join but loses the answer.
	target, _ := url.Parse(srv.URL)
	proxy := httputil.NewSingleHostReverseProxy(target)
	lossy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/join") {
			proxy.ServeHTTP(httptest.NewRecorder(), r)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	defer lossy.Close()
	o := guest.o
	o.RelayURL = lossy.URL
	if _, err := ChatJoin(ctx, rawCode, o); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("lost join response: %v", err)
	}
	if _, err := os.Stat(guest.o.StateDir + "/" + id + ".json"); err != nil {
		t.Fatalf("a 5xx must keep the seat token: %v", err)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatalf("retried join: %v", err)
	}
	if err := ChatSend(ctx, id, "在吗", guest.o); err != nil {
		t.Fatal(err)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "opened,joined,message" {
		t.Fatalf("host after retried join: %v", types(ev))
	}
}

func TestChatCheckpointAndNewGoal(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx := context.Background()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	o := withChat(host.o)
	o.Budget = 2
	if err := Send(ctx, fixture(t, []byte("x")), o); err != nil {
		t.Fatal(err)
	}
	id, rawCode := host.events[0].Chat, host.events[0].Code
	if _, err := Receive(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	if saved := guest.events[len(guest.events)-1]; saved.Budget != 2 {
		t.Fatalf("budget not shown to receiver: %+v", saved)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)

	// The agent reports the goal reached: both sides pause.
	if err := ChatCheckpoint(ctx, id, "字段已对齐：id, name", host.o); err != nil {
		t.Fatal(err)
	}
	ev, _ := guest.recv(ctx, id, time.Second)
	if types(ev) != "checkpoint" || ev[0].By != "peer" || ev[0].Text != "字段已对齐：id, name" || !ev[0].Untrusted {
		t.Fatalf("guest sees checkpoint: %+v", ev)
	}
	if err := ChatSend(ctx, id, "继续", guest.o); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("message while paused: %v", err)
	}
	if err := ChatAccept(ctx, id, host.o); err == nil {
		t.Fatal("accepted with no proposal")
	}
	if err := ChatPropose(ctx, id, "再对 /orders", 2, guest.o); err != nil {
		t.Fatal(err)
	}
	ev, _ = host.recv(ctx, id, time.Second)
	if types(ev) != "proposal" || ev[0].Goal != "再对 /orders" || ev[0].Budget != 2 || !ev[0].Untrusted {
		t.Fatalf("host sees proposal: %+v", ev)
	}
	if err := ChatAccept(ctx, id, host.o); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*side{host, guest} {
		ev, _ := s.recv(ctx, id, time.Second)
		if types(ev) != "resumed" || ev[0].Goal != "再对 /orders" || ev[0].Budget != 2 {
			t.Fatalf("resumed: %+v", ev)
		}
	}
	if ev, _ := guest.recv(ctx, id, 0); types(ev) != "no_events" {
		t.Fatalf("accepted proposal still pending: %v", types(ev))
	}

	// The new budget runs out: the relay pauses without asking the agents.
	for _, text := range []string{"一", "二"} {
		if err := ChatSend(ctx, id, text, host.o); err != nil {
			t.Fatal(err)
		}
	}
	ev, _ = guest.recv(ctx, id, time.Second)
	if types(ev) != "message,message,checkpoint" || ev[2].By != "relay" || ev[2].Reason != "budget" {
		t.Fatalf("budget checkpoint: %+v", ev)
	}
	if ev, _ := host.recv(ctx, id, 0); types(ev) != "checkpoint" || ev[0].By != "relay" {
		t.Fatalf("host sees budget checkpoint: %+v", ev)
	}
	if err := ChatSend(ctx, id, "三", host.o); err == nil || !strings.Contains(err.Error(), "paused") {
		t.Fatalf("message over budget: %v", err)
	}
}
