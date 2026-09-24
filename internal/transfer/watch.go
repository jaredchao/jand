package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// ChatWatch tells a person when the peer has sent something this side's
// agent has not read yet: an agent that cannot be woken by a background
// command needs its user to nudge it.
//
// It only looks. The relay drops every event at or below the "after" a
// reader passes, so the watcher always asks from the agent's own cursor and
// never ahead of it; it keeps no cursor, writes no transcript, and polls
// without waiting so that it never takes one of the room's few long-poll
// slots. Each event is emitted once, marked Unread; the watch returns when
// the chat ends.
func ChatWatch(ctx context.Context, id string, interval time.Duration, opts Options) error {
	o := opts.defaults()
	s, err := loadChat(o.StateDir, id)
	if err != nil {
		return err
	}
	key, err := s.key()
	if err != nil {
		return err
	}
	if interval <= 0 {
		interval = 3 * time.Second
	}
	seen := map[uint64]bool{}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		var cur chatCursor
		readJSONFile(s.path(o.StateDir, ".cursor"), &cur)
		events, err := s.peek(ctx, cur.After)
		if errors.Is(err, ErrChatEnded) || errors.Is(err, ErrChatNotFound) {
			o.Emit(Event{Event: "closed", Chat: s.ID, By: "relay"})
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, e := range events {
			if seen[e.Seq] || e.Seq <= cur.After || e.From == s.Role {
				continue
			}
			seen[e.Seq] = true
			ev := s.describe(key, e)
			o.Emit(ev)
			if ev.Event == "closed" || ev.Event == "expired" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}

// peek returns the events after a cursor without waiting.
func (s *chatState) peek(ctx context.Context, after uint64) ([]relayEvent, error) {
	target, err := relayURL(s.Relay, "/v1/chats/"+s.ID+"/events")
	if err != nil {
		return nil, err
	}
	resp, err := request(ctx, http.MethodGet, target+"?after="+strconv.FormatUint(after, 10)+"&wait=0", s.Token, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot reach relay: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, chatError(resp)
	}
	var page struct {
		Events []relayEvent `json:"events"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page)
	resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("invalid relay response: %w", err)
	}
	return page.Events, nil
}

// describe turns a relay event into what the watcher shows, decrypting what
// it can. Unlike translate it changes no local state.
func (s *chatState) describe(key [32]byte, e relayEvent) Event {
	ev := Event{Event: e.Type, Chat: s.ID, Reason: e.Reason, Budget: e.Budget, Unread: true}
	switch {
	case e.From == "":
		ev.By = "relay"
	case e.From == s.peer():
		ev.By, ev.From = "peer", e.From
	}
	open := func(kind string) (string, bool) {
		if e.Data == "" || e.From != s.peer() {
			return "", false
		}
		text, err := openChat(key, s.ID, e.From, kind, e.Ctr, e.Data)
		return text, err == nil
	}
	switch e.Type {
	case "message":
		text, ok := open("message")
		if !ok {
			return Event{Event: "error", Chat: s.ID, Message: "a message failed authentication; recv will report it"}
		}
		m := parseMessage(text, e.From)
		ev.ID, ev.Kind, ev.ReplyTo, ev.Text, ev.Untrusted = messageID(e.From, e.Ctr), m.Kind, m.ReplyTo, m.Text, true
	case "done", "checkpoint", "declined":
		kind := e.Type
		if kind == "declined" {
			kind = "decline"
		}
		if text, ok := open(kind); ok {
			ev.Text, ev.Untrusted = text, true
		}
	case "proposal":
		if text, ok := open("proposal"); ok {
			ev.Goal, ev.Untrusted = text, true
		}
	}
	return ev
}
