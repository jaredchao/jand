package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jaredchao/jand/docs"
)

var (
	packagingNote = regexp.MustCompile(`(?s)<!-- packaging-note-start -->.*?<!-- packaging-note-end -->\n\n?`)
	modeBlock     = regexp.MustCompile(`(?s)<!-- mode:(\w+) -->\n(.*?)<!-- /mode -->\n`)
	releaseOnly   = regexp.MustCompile(`(?s)<!-- release-only -->\n.*?<!-- /release-only -->\n`)
)

// WakeModes are the ways an agent can wait for chat events: background (its
// host wakes it when a background command ends, as Claude Code does) or poll.
var wakeModes = []string{"background", "poll"}

// renderGuide fills the agent guide template for this machine. With a wake
// mode it keeps only that mode's sections; without one it keeps both.
func renderGuide(exe, relay, mode string) string {
	text := packagingNote.ReplaceAllString(docs.Agent, "")
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
	if len(args) != 1 || args[0] != "agent" {
		fmt.Fprintln(stderr, "usage: jand help [agent]   (agent: the operating guide for agents, filled in for this machine)")
		return 2
	}
	cfg, err := loadClientConfig()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	relay, source := cfg.relayURL("", false)
	fmt.Fprint(out, renderGuide(selfCommand(), relay, cfg.Chat.WakeMode))
	if source == "default" {
		fmt.Fprintf(stderr, "Note: no relay is configured, so the guide names %s. Run jand setup, or set JAND_RELAY.\n", relay)
	}
	return 0
}
