package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jaredchao/jand/internal/transfer"
)

// countWriter counts what a command writes, which is what lands in the
// agent's context when the agent runs it.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// chatMeter collects what one chat command cost this side: which chat it
// was for, the text the agent sent, and the events it printed.
type chatMeter struct {
	id     string
	wrote  int64
	check  bool // recv without --wait
	events []transfer.Event
}

func (m *chatMeter) watch(emit func(transfer.Event)) func(transfer.Event) {
	return func(e transfer.Event) {
		m.events = append(m.events, e)
		emit(e)
	}
}

// own says whether everything printed was this side's own doing.
func (m *chatMeter) own() bool {
	for _, e := range m.events {
		if e.By != "self" {
			return false
		}
	}
	return len(m.events) > 0
}

// record appends the command to the chat's cost record. It is best effort:
// a chat that cannot be found here simply gets no record, and the command's
// own result is never changed by it.
func (m *chatMeter) record(cmd string, out, stderr *countWriter, code int) {
	if m.id == "" {
		return
	}
	r := transfer.CostRecord{Cmd: cmd, Out: out.n + stderr.n, Wrote: m.wrote, Exit: code}
	if cmd == "recv" {
		r.Check = m.check
		r.Own = code == 0 && m.own()
		r.Empty = code == 0 && len(m.events) == 1 && m.events[0].Event == "no_events"
	}
	transfer.RecordCost(transfer.DefaultStateDir(), m.id, r)
}

// costRow is one line of the page's cost table: the whole chat, or one goal.
type costRow struct {
	Label                             string
	Wakes, Empty, Decisions, Messages int
	Out, Wrote, Wall                  string
}

// viewCost is the cost card of chat view. State is "" when the chat was
// recorded from its start, "partial" when recording began later (Since),
// "none" for a chat from before 0.4.4 and "broken" for an unreadable record.
// Without a whole record the page says so instead of showing zeros.
type viewCost struct {
	State, Since, Problem, Outcome string
	Rows                           []costRow
}

// decisions are the commands the protocol runs only with the local user's
// agreement: each one is a point where a person was asked.
var decisions = map[string]bool{"join": true, "propose": true, "accept": true, "close": true, "checkpoint": true}

func buildCost(recs []transfer.CostRecord, rerr error, sections []*viewSection, last time.Time) viewCost {
	var v viewCost
	switch {
	case errors.Is(rerr, os.ErrNotExist) || rerr == nil && len(recs) == 0:
		v.State = "none"
	case rerr != nil:
		v.State, v.Problem = "broken", rerr.Error()
	case recs[0].Cmd != "start" && recs[0].Cmd != "claim":
		t, _ := time.Parse(time.RFC3339, recs[0].Time)
		v.State, v.Since = "partial", localTime(t, "2006-01-02 15:04:05")
	}
	if v.State == "broken" {
		recs = nil
	}
	rows := make([]costRow, len(sections)+1)
	var out, wrote = make([]int64, len(rows)), make([]int64, len(rows))
	rows[0].Label = "整个对话"
	for i, sec := range sections {
		rows[i+1].Label = fmt.Sprintf("目标 %d", sec.N)
		rows[i+1].Messages = sec.Messages
		rows[0].Messages += sec.Messages
		if sec.Outcome != "" {
			v.Outcome = sec.Outcome
		}
	}
	for _, r := range recs {
		t, _ := time.Parse(time.RFC3339, r.Time)
		if t.After(last) {
			last = t
		}
		i := 1
		for j, sec := range sections {
			if !t.Before(sec.start) {
				i = j + 1
			}
		}
		for _, k := range []int{0, i} {
			if k >= len(rows) {
				continue
			}
			if r.Wake() {
				rows[k].Wakes++
				if r.Empty {
					rows[k].Empty++
				}
			}
			if decisions[r.Cmd] && r.Exit == 0 {
				rows[k].Decisions++
			}
			out[k] += r.Out
			wrote[k] += r.Wrote
		}
	}
	for k := range rows {
		rows[k].Out, rows[k].Wrote = sizeText(out[k]), sizeText(wrote[k])
	}
	if len(sections) > 0 {
		rows[0].Wall = durationText(last.Sub(sections[0].start))
		for i, sec := range sections {
			end := last
			if i+1 < len(sections) {
				end = sections[i+1].start
			}
			rows[i+1].Wall = durationText(end.Sub(sec.start))
		}
	}
	v.Rows = rows
	return v
}

func sizeText(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}

func durationText(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < 0:
		return "-"
	case d < time.Minute:
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}
