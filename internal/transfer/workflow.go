package transfer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

// A Workflow is how two parties agree to work in one chat. The host picks it;
// it travels to the guest in the encrypted charter, and the guest's user
// accepts it together with the goal. The relay enforces DoneRule, PauseNotes
// and Budget; clients enforce Kinds. Roles and Instructions are shown only,
// and are untrusted text like everything else the peer sends.
type Workflow struct {
	Name  string `json:"name"`
	Title string `json:"title,omitempty"`
	// DoneRule is "all" (pause once both sides report done) or "any" (pause
	// at the first done: one side works, the other accepts).
	DoneRule string `json:"done_rule"`
	// PauseNotes is the closing messages each side may send while paused;
	// nil means the relay's default.
	PauseNotes *int `json:"pause_notes,omitempty"`
	// Budget is the first goal's budget; 0 means the local or relay default.
	Budget       int               `json:"budget,omitempty"`
	Kinds        []string          `json:"kinds,omitempty"`
	Roles        map[string]string `json:"roles,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
}

const (
	maxInstructions = 8 * 1024
	// MaxPauseNotes matches the relay's hard ceiling; a relay may allow fewer.
	MaxPauseNotes = 10
)

// DefaultWorkflow is what a chat without --workflow uses: 0.4.0 behaviour.
func DefaultWorkflow() Workflow {
	return Workflow{Name: "default", DoneRule: "all", Kinds: slices.Clone(MessageKinds)}
}

// HomeDir is JAND_HOME, or jand under the user configuration directory.
func HomeDir() string {
	if v := os.Getenv("JAND_HOME"); v != "" {
		return v
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "jand")
	}
	return ".jand"
}

// WorkflowDir holds this machine's workflow files, <name>.json.
func WorkflowDir() string {
	return filepath.Join(HomeDir(), "workflows")
}

func (w *Workflow) Validate() error {
	if w.Name == "" || len(w.Name) > 64 || strings.ContainsAny(w.Name, `/\`) {
		return errors.New("workflow needs a short name without slashes")
	}
	if w.DoneRule == "" {
		w.DoneRule = "all"
	}
	if w.DoneRule != "all" && w.DoneRule != "any" {
		return fmt.Errorf("workflow %s: done_rule must be \"all\" or \"any\"", w.Name)
	}
	if w.PauseNotes != nil && (*w.PauseNotes < 0 || *w.PauseNotes > MaxPauseNotes) {
		return fmt.Errorf("workflow %s: pause_notes must be between 0 and %d", w.Name, MaxPauseNotes)
	}
	if w.Budget < 0 || w.Budget > MaxChatBudget {
		return fmt.Errorf("workflow %s: budget must be between 1 and %d", w.Name, MaxChatBudget)
	}
	if len(w.Kinds) == 0 {
		w.Kinds = slices.Clone(MessageKinds)
	}
	for _, k := range w.Kinds {
		if !knownKind(k) {
			return fmt.Errorf("workflow %s: unknown message kind %q; kinds must come from %s", w.Name, k, strings.Join(MessageKinds, ", "))
		}
	}
	// A request nobody may answer would deadlock the chat.
	if slices.Contains(w.Kinds, "request") && !slices.Contains(w.Kinds, "reply") {
		return fmt.Errorf("workflow %s: a workflow that allows request must allow reply", w.Name)
	}
	for role := range w.Roles {
		if role != "host" && role != "guest" {
			return fmt.Errorf("workflow %s: roles may only describe host and guest", w.Name)
		}
	}
	text := w.Title + w.Instructions
	for _, r := range w.Roles {
		text += r
	}
	if len(w.Instructions) > maxInstructions || !utf8.ValidString(text) {
		return fmt.Errorf("workflow %s: instructions must be valid UTF-8 of at most %d bytes", w.Name, maxInstructions)
	}
	return nil
}

func (w *Workflow) allows(kind string) bool {
	return w == nil || slices.Contains(w.Kinds, kind)
}

// LoadWorkflow resolves "default", a name in WorkflowDir, or a path to a
// .json file. Unknown fields are errors, so a mistyped rule is not ignored.
func LoadWorkflow(ref string) (Workflow, error) {
	if ref == "" || ref == "default" {
		return DefaultWorkflow(), nil
	}
	path := ref
	if !strings.ContainsAny(ref, `/\`) && !strings.HasSuffix(ref, ".json") {
		path = filepath.Join(WorkflowDir(), ref+".json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Workflow{}, fmt.Errorf("workflow %s: %w", ref, err)
	}
	var w Workflow
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Workflow{}, fmt.Errorf("workflow %s: %w", path, err)
	}
	return w, w.Validate()
}

// charter is what the host seals for the guest at room creation: the goal
// and the workflow, authenticated by the chat key so the relay cannot alter
// them. The relay separately holds the parameters it enforces, in the clear;
// the guest checks the two agree before joining.
type charter struct {
	Protocol string   `json:"protocol"`
	Goal     string   `json:"goal"`
	Budget   int      `json:"budget,omitempty"`
	Workflow Workflow `json:"workflow"`
}

const charterProtocol = "jand-charter/1"

// relayTerms are the enforced parameters the relay reports for a room.
type relayTerms struct {
	Charter    string `json:"charter"`
	DoneRule   string `json:"done_rule"`
	PauseNotes int    `json:"pause_notes"`
	Budget     int    `json:"budget"`
}

// matches reports whether the relay enforces what the sealed charter says.
func (c charter) matches(t relayTerms) error {
	if c.Workflow.DoneRule != t.DoneRule {
		return fmt.Errorf("relay enforces done_rule %q but the charter says %q", t.DoneRule, c.Workflow.DoneRule)
	}
	if c.Workflow.PauseNotes != nil && *c.Workflow.PauseNotes != t.PauseNotes {
		return fmt.Errorf("relay allows %d closing notes but the charter says %d", t.PauseNotes, *c.Workflow.PauseNotes)
	}
	if c.Budget != 0 && c.Budget != t.Budget {
		return fmt.Errorf("relay enforces a budget of %d but the charter says %d", t.Budget, c.Budget)
	}
	return nil
}
