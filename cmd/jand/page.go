package main

import (
	"cmp"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/jaredchao/jand/internal/transfer"
)

// The chat page is chat view's page, kept at $JAND_HOME/chats/<id>.html and
// rewritten after every chat command, so the person can watch the chat
// without asking for it. It is opened once, when this side starts the chat
// or joins it. chat.page in config.json, or JAND_PAGE, chooses: open (the
// default), update (keep it current, never open a browser) or off.
var pageModes = []string{"open", "update", "off"}

// pageMode applies JAND_PAGE > config > open. An unknown JAND_PAGE never
// opens a browser: someone who set it wanted less, not more.
func (c clientConfig) pageMode() string {
	if v := os.Getenv("JAND_PAGE"); v != "" {
		if slices.Contains(pageModes, v) {
			return v
		}
		return "update"
	}
	return cmp.Or(c.Chat.Page, "open")
}

// openPage is replaced in tests so they never start a browser.
var openPage = openBrowser

// refreshPage rewrites the chat's page and, if show is set and the person
// allows it, opens it. It is best effort: the command's own result never
// depends on it, and nothing is printed, so the agent's context is unchanged.
func refreshPage(id string, show bool) {
	if id == "" {
		return
	}
	cfg, err := loadClientConfig()
	if err != nil || cfg.pageMode() == "off" {
		return
	}
	dir := transfer.DefaultStateDir()
	summary, lines, err := transfer.ChatHistory(dir, id)
	if err != nil {
		return
	}
	recs, cerr := transfer.ChatCost(dir, summary.ID)
	path := filepath.Join(dir, summary.ID+".html")
	if writeView(path, summary, lines, recs, cerr) != nil {
		return
	}
	if show && cfg.pageMode() == "open" && hasDisplay() {
		openPage(path)
	}
}

// hasDisplay is false on a Linux or BSD machine with no desktop session,
// such as a server reached over ssh, where there is nobody to show it to.
func hasDisplay() bool {
	switch runtime.GOOS {
	case "darwin", "windows":
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
