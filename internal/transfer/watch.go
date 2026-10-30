package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
	return chatWatch(ctx, id, interval, opts, nil)
}

// ChatWatchNotify watches without consuming messages and asks a host to notify
// the bound agent. One notification covers the new actionable events in a poll.
// A failed notification is retried with backoff; success means queued, not read.
// Receipts live only for this process: restarting may remind about unread events.
func ChatWatchNotify(ctx context.Context, id string, interval time.Duration, opts Options, notify func(context.Context, string) error) error {
	if notify == nil {
		return errors.New("agent notifier is required")
	}
	return chatWatch(ctx, id, interval, opts, notify)
}

func chatWatch(ctx context.Context, id string, interval time.Duration, opts Options, notify func(context.Context, string) error) error {
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
	notified := map[uint64]bool{}
	var nextAttempt time.Time
	delay := interval
	relayDelay := interval
	ended := false
	pending := false
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		var cur chatCursor
		if err := readJSONFile(s.path(o.StateDir, ".cursor"), &cur); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot read agent cursor: %w", err)
		}
		var events []relayEvent
		roomEnded := false
		if !ended {
			var page watchPage
			page, err = s.peekPage(ctx, cur.After)
			events, roomEnded = page.Events, page.State == "ended"
		}
		if errors.Is(err, ErrChatEnded) || errors.Is(err, ErrChatNotFound) {
			if notify != nil {
				return err // Gone is not proof that the agent received a closing event.
			}
			o.Emit(Event{Event: "closed", Chat: s.ID, By: "relay"})
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				if notify != nil {
					return ctx.Err()
				}
				return nil
			}
			var temporary *watchTransientError
			if notify != nil && errors.As(err, &temporary) {
				o.Emit(Event{Event: "watch_retry", Chat: s.ID, Message: err.Error()})
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(relayDelay):
				}
				relayDelay = min(relayDelay*2, 30*time.Second)
				continue
			}
			return err
		}
		relayDelay = interval
		if !ended {
			pending = false
		}
		var batch []uint64
		for _, e := range events {
			if e.Seq <= cur.After || e.From == s.Role {
				continue
			}
			ev := s.describe(key, e)
			if !seen[e.Seq] {
				seen[e.Seq] = true
				o.Emit(ev)
			}
			if !notified[e.Seq] && (ev.Event != "message" || ev.Kind != "progress") {
				pending = true
				batch = append(batch, e.Seq)
			}
			if ev.Event == "closed" || ev.Event == "expired" || ev.Event == "declined" {
				ended = true
			}
		}
		ended = ended || roomEnded
		if notify != nil && pending && !time.Now().Before(nextAttempt) {
			if err := notify(ctx, s.ID); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				o.Emit(Event{Event: "agent_notify_retry", Chat: s.ID, Message: err.Error()})
				nextAttempt = time.Now().Add(delay)
				delay = min(delay*2, 30*time.Second)
			} else {
				for _, seq := range batch {
					notified[seq] = true
				}
				pending = false
				delay, nextAttempt = interval, time.Time{}
			}
		}
		if ended && (notify == nil || !pending) {
			return nil
		}
		select {
		case <-ctx.Done():
			if notify != nil {
				return ctx.Err()
			}
			return nil
		case <-tick.C:
		}
	}
}

// Only transport failures, throttling and server failures are retryable.
// Authentication, missing rooms and malformed responses remain failures.
type watchTransientError struct{ error }

func (e *watchTransientError) Unwrap() error { return e.error }

// peek returns the events after a cursor without waiting.
func (s *chatState) peek(ctx context.Context, after uint64) ([]relayEvent, error) {
	page, err := s.peekPage(ctx, after)
	return page.Events, err
}

type watchPage struct {
	State  string       `json:"state"`
	Events []relayEvent `json:"events"`
}

func (s *chatState) peekPage(ctx context.Context, after uint64) (watchPage, error) {
	var page watchPage
	target, err := relayURL(s.Relay, "/v1/chats/"+s.ID+"/events")
	if err != nil {
		return page, err
	}
	resp, err := request(ctx, http.MethodGet, target+"?after="+strconv.FormatUint(after, 10)+"&wait=0", s.Token, nil)
	if err != nil {
		return page, &watchTransientError{fmt.Errorf("cannot reach relay: %w", err)}
	}
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return page, &watchTransientError{chatError(resp)}
		}
		return page, chatError(resp)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&page)
	resp.Body.Close()
	if err != nil {
		return page, fmt.Errorf("invalid relay response: %w", err)
	}
	return page, nil
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
