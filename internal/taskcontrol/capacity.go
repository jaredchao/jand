package taskcontrol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
)

// Host-wide limits bound all goal phases sharing this controller directory.
// Explicit local controller.json can raise them. Unknown records retain slots.
type capacity struct {
	Running      int `json:"running"`
	Queued       int `json:"queued"`
	Coordinators int `json:"coordinators"`
}

var ledgerName = regexp.MustCompile(`^[a-f0-9]{64}\.json$`)

func headroom(root string, current *State) (capacity, error) {
	limit := capacity{1, 8, 1}
	b, e := os.ReadFile(filepath.Join(root, "controller.json"))
	if e == nil {
		if e = json.Unmarshal(b, &limit); e != nil {
			return limit, e
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return limit, e
	}
	if limit.Running < 1 || limit.Running > 16 || limit.Queued < 1 || limit.Queued > 1024 || limit.Coordinators < 1 || limit.Coordinators > 8 {
		return limit, errors.New("invalid host-wide controller capacity")
	}
	files, e := os.ReadDir(root)
	if e != nil {
		return limit, e
	}
	for _, file := range files {
		if !ledgerName.MatchString(file.Name()) || file.Name() == Key(current.Scope)+".json" {
			continue
		}
		data, e := os.ReadFile(filepath.Join(root, file.Name()))
		if e != nil {
			return limit, e
		}
		var s State
		if e = json.Unmarshal(data, &s); e != nil {
			return limit, errors.New("another task scope is corrupt; new admissions blocked")
		}
		if e = s.validate(); e != nil {
			return limit, e
		}
		r, q := s.counts()
		limit.Running -= r
		limit.Queued -= q
		if s.Coordinator != nil && s.Coordinator.State != "stopped" {
			limit.Coordinators--
		}
	}
	return limit, nil
}
