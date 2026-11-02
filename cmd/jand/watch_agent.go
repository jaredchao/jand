package main

import (
	"context"
	"errors"
	"io"
	"regexp"

	"github.com/jaredchao/jand/internal/transfer"
)

var threadUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

var errCodexQueueReceiptsUnavailable = errors.New("automatic Codex reminders are paused: host queue processing cannot be verified; run jand chat recv manually")

// Queue acceptance, a relay cursor and an idle thread are not host processing
// receipts. The installed CLI inserts notices but exposes no processing ack.
// Until a same-host adapter can verify that lifecycle, fail BEFORE any queue
// mutation, including on fresh starts, concurrent watchers and restarts.
// Do not add a timeout/cursor/manual-recv bypass or retry an uncertain add.
func codexWatchNotifier(agent, thread string, _ bool, _ io.Writer) (func(context.Context, string, transfer.WatchNotice) error, error) {
	if agent != "codex" || !threadUUID.MatchString(thread) {
		return nil, errors.New("agent watch requires --agent codex --thread <exact session UUID>")
	}
	return nil, errCodexQueueReceiptsUnavailable
}
