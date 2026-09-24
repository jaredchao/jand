package transfer

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ChatSummary describes one local chat for listing and review. It is built
// from this side's state and transcript only and never carries the chat's
// token or key.
type ChatSummary struct {
	ID       string `json:"chat"`
	Role     string `json:"role"`
	Relay    string `json:"relay"`
	Goal     string `json:"goal,omitempty"` // the current goal; empty if never recorded
	Budget   int    `json:"budget,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	// Status is waiting (host, peer not joined), active, paused (at a
	// checkpoint), ended (closed, expired or declined), or unknown (an
	// older chat with no transcript).
	Status   string    `json:"status"`
	Started  time.Time `json:"started,omitempty"`
	Last     time.Time `json:"last,omitempty"`
	Messages int       `json:"messages"`
}

// ChatList summarizes every chat kept in dir, most recently active first.
func ChatList(dir string) ([]ChatSummary, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var list []ChatSummary
	for _, m := range matches {
		id := strings.TrimSuffix(filepath.Base(m), ".json")
		if len(id) != 32 {
			continue
		}
		s, err := loadChat(dir, id)
		if err != nil {
			continue
		}
		lines, _ := readTranscript(s.path(dir, ".transcript.jsonl"))
		c := summarize(s, lines)
		if len(lines) == 0 {
			// Chats from before 0.4.3 that never exchanged anything have no
			// transcript: their state is unknown, and the state file's time
			// is the best date there is.
			c.Status = "unknown"
			if info, err := os.Stat(m); err == nil {
				c.Started, c.Last = info.ModTime().UTC(), info.ModTime().UTC()
			}
		}
		list = append(list, c)
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Last.After(list[j].Last) })
	return list, nil
}

// ChatHistory returns one chat's summary and its whole transcript.
func ChatHistory(dir, id string) (ChatSummary, []TranscriptLine, error) {
	s, err := loadChat(dir, id)
	if err != nil {
		return ChatSummary{}, nil, err
	}
	lines, err := readTranscript(s.path(dir, ".transcript.jsonl"))
	if err != nil {
		return ChatSummary{}, nil, err
	}
	return summarize(s, lines), lines, nil
}

// TranscriptPath is where a chat's transcript is kept, for following it.
func TranscriptPath(dir, id string) (string, error) {
	s, err := loadChat(dir, id)
	if err != nil {
		return "", err
	}
	return s.path(dir, ".transcript.jsonl"), nil
}

// ParseTranscriptLine reads one transcript line; ok is false for a line
// that is not a transcript entry (such as a partly written one).
func ParseTranscriptLine(b []byte) (TranscriptLine, bool) {
	var l TranscriptLine
	if json.Unmarshal(b, &l) != nil || l.Time == "" || l.Kind == "" {
		return l, false
	}
	return l, true
}

func readTranscript(path string) ([]TranscriptLine, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []TranscriptLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // one message is at most 64 KiB, JSON-escaped
	for sc.Scan() {
		if l, ok := ParseTranscriptLine(sc.Bytes()); ok {
			lines = append(lines, l)
		}
	}
	return lines, sc.Err()
}

func summarize(s *chatState, lines []TranscriptLine) ChatSummary {
	c := ChatSummary{ID: s.ID, Role: s.Role, Relay: s.Relay, Status: "active"}
	if s.Workflow != nil {
		c.Workflow = s.Workflow.Name
	}
	if s.Role == "host" {
		c.Status = "waiting"
	}
	for i, l := range lines {
		t, _ := time.Parse(time.RFC3339, l.Time)
		if i == 0 {
			c.Started = t
		}
		c.Last = t
		switch l.Kind {
		case "started", "joined":
			if l.Kind == "joined" && s.Role == "host" {
				c.Status = "active" // the guest joined; the goal came with started
				continue
			}
			c.Goal, c.Budget = l.Text, l.Budget
			if l.Workflow != "" {
				c.Workflow = l.Workflow
			}
			if l.Kind == "joined" {
				c.Status = "active"
			}
		case "message":
			c.Messages++
		case "checkpoint":
			c.Status = "paused"
		case "resumed":
			c.Goal, c.Budget, c.Status = l.Text, l.Budget, "active"
		case "closed", "expired", "decline":
			c.Status = "ended"
		}
	}
	return c
}
