package transfer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jaredchao/jand/internal/relay"
)

func TestChatHistoryFollowsTheChat(t *testing.T) {
	_, srv := server(t, relay.DefaultConfig())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	host, guest := newSide(t, srv.URL), newSide(t, srv.URL)
	id, rawCode := invite(t, ctx, host, guest)

	status := func(s *side) ChatSummary {
		t.Helper()
		c, _, err := ChatHistory(s.o.StateDir, id)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := status(host); c.Status != "waiting" || c.Goal != testGoal || c.Workflow != "default" {
		t.Fatalf("host before join: %+v", c)
	}
	if _, err := ChatJoin(ctx, rawCode, guest.o); err != nil {
		t.Fatal(err)
	}
	// The guest's record starts with the goal it agreed to.
	_, lines, _ := ChatHistory(guest.o.StateDir, id)
	if len(lines) != 1 || lines[0].Kind != "joined" || lines[0].Text != testGoal || lines[0].Workflow != "default" {
		t.Fatalf("guest transcript: %+v", lines)
	}
	host.recv(ctx, id, 0)
	if c := status(host); c.Status != "active" {
		t.Fatalf("host after join: %+v", c)
	}
	if err := ChatSend(ctx, id, ChatMessage{Kind: "request", Text: "字段？"}, host.o); err != nil {
		t.Fatal(err)
	}
	guest.recv(ctx, id, 0)
	if err := ChatDone(ctx, id, "ok", guest.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)
	if err := ChatDone(ctx, id, "ok", host.o); err != nil {
		t.Fatal(err)
	}
	host.recv(ctx, id, 0)
	if c := status(host); c.Status != "paused" || c.Messages != 1 {
		t.Fatalf("host at checkpoint: %+v", c)
	}
	if err := ChatClose(ctx, id, host.o); err != nil {
		t.Fatal(err)
	}
	list, err := ChatList(host.o.StateDir)
	if err != nil || len(list) != 1 || list[0].Status != "ended" || list[0].Started.IsZero() || list[0].Last.Before(list[0].Started) {
		t.Fatalf("list: %+v %v", list, err)
	}
	// A summary is shown to people: it must never carry the chat's secrets.
	s, _ := loadChat(host.o.StateDir, id)
	shown, _ := json.Marshal(list)
	if strings.Contains(string(shown), s.Token) || strings.Contains(string(shown), s.Key) {
		t.Fatalf("summary leaks a secret: %s", shown)
	}
}
