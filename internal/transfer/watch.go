package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
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
// the bound agent. Adjacent polls coalesce for two quiet intervals, bounded by
// four intervals of continuous activity. Later events can notify again even
// while an earlier queued notice remains unread.
// A failed notification is retried with backoff; success means queued, not read.
// Receipts live only for this process: restarting may remind about unread events.
func ChatWatchNotify(ctx context.Context, id string, interval time.Duration, opts Options, notify func(context.Context, string) error) error {
	if notify == nil {
		return errors.New("agent notifier is required")
	}
	return ChatWatchNotifyBatch(ctx, id, interval, opts, func(ctx context.Context, chat string, _ WatchNotice) error {
		return notify(ctx, chat)
	})
}

// WatchNotice identifies a batch using only sequence numbers and a whitelist of
// event categories. It never includes peer text, goals, keys or capabilities.
type WatchNotice struct {
	FirstSeq   uint64   `json:"first_seq"`
	LastSeq    uint64   `json:"last_seq"`
	Count      int      `json:"count"`
	Categories []string `json:"categories"`
}

// ChatWatchNotifyBatch is ChatWatchNotify with explicit, non-secret batch metadata.
func ChatWatchNotifyBatch(ctx context.Context, id string, interval time.Duration, opts Options, notify func(context.Context, string, WatchNotice) error) error {
	if notify == nil {
		return errors.New("agent notifier is required")
	}
	return chatWatch(ctx, id, interval, opts, notify)
}

func chatWatch(ctx context.Context, id string, interval time.Duration, opts Options, notify func(context.Context, string, WatchNotice) error) error {
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
	var batch watchNoticeBatch
	relayDelay := interval
	ended := false
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		var cur chatCursor
		if err := readJSONFile(s.path(o.StateDir, ".cursor"), &cur); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cannot read agent cursor: %w", err)
		}
		batch.discardThrough(cur.After)
		for seq := range seen {
			if seq <= cur.After {
				delete(seen, seq)
			}
		}
		for seq := range notified {
			if seq <= cur.After {
				delete(notified, seq)
			}
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
		now := time.Now()
		for _, e := range events {
			if e.Seq <= cur.After || e.From == s.Role {
				continue
			}
			ev := s.describe(key, e)
			if !seen[e.Seq] {
				seen[e.Seq] = true
				o.Emit(ev)
			}
			if notify != nil && !notified[e.Seq] && (ev.Event != "message" || ev.Kind != "progress") {
				batch.add(e.Seq, watchCategory(ev), now)
			}
			if ev.Event == "closed" || ev.Event == "expired" || ev.Event == "declined" {
				ended = true
			}
		}
		ended = ended || roomEnded
		if notify != nil && batch.ready(now, interval, ended) {
			if err := notify(ctx, s.ID, batch.notice()); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				o.Emit(Event{Event: "agent_notify_retry", Chat: s.ID, Message: err.Error()})
				batch.failed(time.Now(), interval)
			} else {
				for seq := range batch.events {
					notified[seq] = true
				}
				batch = watchNoticeBatch{}
			}
		}
		if ended && (notify == nil || len(batch.events) == 0) {
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

// All state belongs to this watcher process. A restart may remind about unread
// events again; queued is not consumed and cannot establish exactly-once delivery.
type watchNoticeBatch struct {
	events               map[uint64]string
	first, last, retryAt time.Time
	retryDelay           time.Duration
	attempted            bool
}

func (b *watchNoticeBatch) add(seq uint64, category string, now time.Time) {
	if b.events == nil {
		b.events = map[uint64]string{}
	}
	if _, ok := b.events[seq]; ok {
		return
	}
	if len(b.events) == 0 {
		b.first = now
	}
	b.events[seq] = category
	b.last = now
}

func (b *watchNoticeBatch) discardThrough(after uint64) {
	for seq := range b.events {
		if seq <= after {
			delete(b.events, seq)
		}
	}
	if len(b.events) == 0 {
		*b = watchNoticeBatch{}
	}
}

func (b *watchNoticeBatch) ready(now time.Time, interval time.Duration, ended bool) bool {
	return len(b.events) > 0 && !now.Before(b.retryAt) && (b.attempted || ended || !now.Before(b.last.Add(2*interval)) || !now.Before(b.first.Add(4*interval)))
}

func (b *watchNoticeBatch) failed(now time.Time, interval time.Duration) {
	if b.retryDelay == 0 {
		b.retryDelay = interval
	}
	b.retryAt = now.Add(b.retryDelay)
	b.retryDelay = min(b.retryDelay*2, 30*time.Second)
	b.attempted = true
}

func (b *watchNoticeBatch) notice() WatchNotice {
	n := WatchNotice{Count: len(b.events), Categories: []string{}}
	categories := map[string]bool{}
	for seq, category := range b.events {
		if n.FirstSeq == 0 || seq < n.FirstSeq {
			n.FirstSeq = seq
		}
		n.LastSeq = max(n.LastSeq, seq)
		categories[category] = true
	}
	for c := range categories {
		n.Categories = append(n.Categories, c)
	}
	slices.Sort(n.Categories)
	return n
}

func watchCategory(ev Event) string {
	if ev.Event == "message" {
		if slices.Contains(MessageKinds, ev.Kind) {
			return "message/" + ev.Kind
		}
		return "message/unknown"
	}
	if slices.Contains([]string{"opened", "joined", "done", "checkpoint", "proposal", "resumed", "closed", "expired", "declined", "error"}, ev.Event) {
		return ev.Event
	}
	return "event/unknown"
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
