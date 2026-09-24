package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jaredchao/jand/docs"
)

var (
	packagingNote = regexp.MustCompile(`(?s)<!-- packaging-note-start -->.*?<!-- packaging-note-end -->\n\n?`)
	modeBlock     = regexp.MustCompile(`(?s)<!-- mode:(\w+) -->\n(.*?)<!-- /mode -->\n`)
	releaseOnly   = regexp.MustCompile(`(?s)<!-- release-only -->\n.*?<!-- /release-only -->\n`)
	topicMarker   = regexp.MustCompile(`(?m)^<!-- topic:(\w+) -->\n`)
)

// WakeModes are the ways an agent can wait for chat events: background (its
// host wakes it when a background command ends, as Claude Code does) or poll.
var wakeModes = []string{"background", "poll"}

// guideTopics are the sections of the agent guide. core is printed by
// default: the quick reference and every safety rule. The rest is read when
// the work needs it, which keeps a plain receive from costing the whole guide.
var guideTopics = []string{"core", "send", "receive", "chat", "host"}

// renderGuide fills the agent guide template for this machine. With a wake
// mode it keeps only that mode's sections; without one it keeps both. topic
// selects one section, or "all".
func renderGuide(exe, relay, mode, topic string) string {
	text := docs.Agent
	if topic != "all" {
		head := text[:topicMarker.FindStringIndex(text)[0]]
		parts := topicMarker.Split(text, -1)[1:]
		names := topicMarker.FindAllStringSubmatch(text, -1)
		text = head
		for i, m := range names {
			if m[1] == topic {
				if topic != "core" {
					text = "# jand · 给 Agent 的使用说明（" + topic + "）\n\n"
				}
				text += parts[i]
			}
		}
	}
	text = topicMarker.ReplaceAllString(text, "")
	text = packagingNote.ReplaceAllString(text, "")
	text = releaseOnly.ReplaceAllString(text, "")
	text = modeBlock.ReplaceAllStringFunc(text, func(block string) string {
		m := modeBlock.FindStringSubmatch(block)
		if mode == "" || m[1] == mode {
			return m[2]
		}
		return ""
	})
	return strings.NewReplacer("__JAND__", exe, "__RELAY_URL__", relay).Replace(text)
}

// selfCommand is how an agent on this machine should invoke this program:
// plain "jand" when that is what PATH finds, otherwise the full path.
func selfCommand() string {
	self, err := os.Executable()
	if err != nil {
		return "jand"
	}
	if real, err := filepath.EvalSymlinks(self); err == nil {
		self = real
	}
	if onPath, err := exec.LookPath("jand"); err == nil {
		if real, err := filepath.EvalSymlinks(onPath); err == nil && real == self {
			return "jand"
		}
	}
	if strings.ContainsAny(self, " \t") {
		return `"` + self + `"`
	}
	return self
}

// helpTopic serves jand help <topic>. The only topic is agent: the guide an
// agent reads before using jand, which is too long to be the usage text.
func helpTopic(args []string, out, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "template" {
		fmt.Fprint(out, docs.Template)
		return 0
	}
	topic := "core"
	if len(args) == 2 && args[0] == "agent" && (args[1] == "all" || slices.Contains(guideTopics[1:], args[1])) {
		topic = args[1]
	} else if len(args) != 1 || args[0] != "agent" {
		fmt.Fprintln(stderr, "usage: jand help agent [send|receive|chat|host|all] | jand help template")
		return 2
	}
	cfg, err := loadClientConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	relay, source := cfg.relayURL("", false)
	fmt.Fprint(out, renderGuide(selfCommand(), relay, cfg.Chat.WakeMode, topic))
	if source == "default" {
		fmt.Fprintf(stderr, "Note: no relay is configured, so the guide names %s. Run jand setup, or set JAND_RELAY.\n", relay)
	}
	return 0
}
