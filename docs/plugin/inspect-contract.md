# Native chat inspection — schema 1

Status: included in Jand 0.5.0. The older v0.4.6 does not contain this command.
Plugin integration detects the actual help capability before invocation; no Relay
protocol change is required for inspection.

```
jand chat inspect --json <chat-id-or-unambiguous-8+-hex-prefix>
```

Returns one JSON object and newline. Exit 0 means a snapshot was exported;
**it does not mean records are complete**. Consumers must check the integrity
fields. Exit 1: chat/state/read/output error (stderr); exit 2: invalid invocation.
Missing or damaged transcript/cost files are represented in the JSON, separately
from a missing/unreadable chat state, which fails the command.

## Read-only and security contract

Only reads this machine's `$JAND_HOME/chats` files. Never contacts Relay, loads or
advances recv cursors, sends/receives messages, refreshes HTML, opens a browser,
or appends a cost record. It does not fetch peer activity that has not been
received locally. A successful export is not evidence of current Relay status.
Files are read independently; this is not a transactional snapshot across a
concurrently running send/recv. Re-read after those commands finish for a stable
boundary. Individual input files are capped at 64 MiB; larger files are marked
broken rather than exported as complete.

Chat state uses a field whitelist. Protocol token, encryption key, capability,
cursor and config are never serialized. `chat.relay` is an origin only (no URL
credentials, path, query or fragment). Transcript text, goals and summaries are
user-authored content, exported verbatim; they may themselves contain private
information. Render as untrusted text, never raw HTML, authorization or tool
instructions. Do not upload snapshots automatically.

## Safe capability detection with published 0.4.6

Do not probe an unknown chat subcommand against an existing chat on the old
0.4.6 client: despite returning exit 2, its generic meter appends a cost record
(and can refresh HTML). Probe `jand chat --help` without a chat ID first; it has
no per-chat side effects. Call native inspect on live history only when help
explicitly advertises `jand chat inspect --json`, then validate schema 1. An
isolated disposable home is another safe place to test an unknown command.
The old client remains on the whitelist local-file projection. Version text
alone is insufficient: the local development build also reports 0.4.6.

## Fields

| Field | Meaning |
| --- | --- |
| `schema_version` | Integer 1. Reject unsupported schema versions. |
| `scope` | `local`; never describes an authoritative live Relay snapshot. |
| `chat` | Existing safe summary: chat ID, role, relay origin, current goal/budget/workflow, status, started/last RFC3339 timestamps and total recorded message count. |
| `transcript_integrity` | Recording coverage (see below). `chat.status=unknown` unless this is complete. Other derived transcript fields describe the valid prefix only. |
| `timeline` | Typed transcript lines in original order, no clipping: time/from/kind/id/type/reply_to/supersedes/text/budget/workflow/plan/step/mode as applicable. |
| `sections` | Goal segments, starting at first recorded line or `resumed`: n, goal, budget, started, messages, outcome, timeline_indexes, summary_indexes, deliveries, cost, cost_state, wall_seconds. Indexes are zero-based into timeline; n starts at 1. Outcome is checkpoint/closed/expired/decline when observed. Summary indexes select `done` events. Deliveries are IDs of non-superseded deliveries. |
| `messages` | Message projections with id/section/from/kind/text, reply_to, answers[], pending, supersedes and superseded_by. Every message kind with explicit reply_to links the referenced peer message (including a closing note). Pending means no recorded reply, not a business verdict. |
| `plan` | State's agreed steps[] (goals after the first), local advance mode (default ask), latest pending proposal and checkpoint transcript events or null. Resume clears pending proposal/checkpoint. No proposal acceptance or auto-advance is performed. |
| `cost` | integrity, raw records[], totals or null, wall_seconds or null. Section cost uses the same numeric totals and each section's interval. |

Collections are always arrays, including empty arrays. Optional transcript and
message linkage fields can be omitted. Null totals/time are unknown/unavailable,
not zero. Fields can be added within schema 1; changing meanings requires a new
schema version. A consumer should ignore unknown added fields.

## Integrity

`{state, reason?, valid_lines, problem_line?, started_late?}` independently for transcript/cost:

- `complete`: all terminated records parsed, beginning at started/joined for
  transcript or start/claim for cost. This describes local recording coverage;
  it does not prove the peer is current or detect missing whole valid lines.
- `partial`: recording began later (`late_recording`) or file ends in an
  unterminated line (`unterminated_tail`). The unterminated line is excluded,
  even if it happens to parse. `started_late=true` preserves a late start even
  when an unterminated tail also exists. Valid-prefix cost totals are usable only as
  explicitly partial totals. Re-read after an active writer finishes.
- `missing`: file absent. `empty`: present with no bytes.
- `broken`: unreadable, too large, invalid JSON/record, negative byte counters,
  invalid timestamp, missing/null required fields, or numeric overflow. Valid prefix
  may still appear for diagnosis; **cost.totals and all section costs are null**.
  Problems use codes/line numbers and never echo the damaged source text.

Section costs are null unless transcript coverage is complete and a usable cost
prefix exists. `sections[].cost_state` inherits the cost integrity state (or
`unavailable` for an incomplete transcript). Segments entirely before late cost
recording began have null cost and state=missing, never a false zero. Cost commands before the first transcript timestamp contribute to
the whole-chat total, not a goal segment. Commands at a new segment's start
belong to the new segment. Local wall_seconds spans first segment start to the
later of last transcript/cost timestamp; it includes waiting, not CPU time.
When transcript coverage is incomplete, derived wall time is null. Broken cost
records cannot extend the observed transcript span.

## Cost meaning (unchanged)

- `wakes`: original `CostRecord.Wake()`: cmd=recv and neither check nor own.
  A timed-out waiting recv and a failing/ended waiting recv still count. A
  non-waiting check and recv containing only own actions do not. This measures
  jand CLI returns under this rule, **not actual DSH host wakeups**.
- `empty_wakes`: counted wake records with empty=true.
- `decisions`: successful join/propose/accept/close/checkpoint command records.
  This records protocol decision points; it does not prove a person clicked.
- `out_bytes`: sum of stdout+stderr bytes for recorded jand commands, including
  failed commands. `wrote_bytes`: sum of the recorded sent-text bytes.
- Raw `records` preserve time/cmd/out/wrote/empty/check/own/exit. Bytes are not
  tokens and must not be converted to a token estimate. Plugin host wake/tool
  output metrics are separate measurements with their own provenance.

## Validation

`go test -race ./... -skip '^TestAppleScriptQuoting$'` covers new snapshot
projection, linkage, goal boundaries, cost rules, missing/empty/partial/broken
inputs and CLI file-write guards. `python3 scripts/plugin_smoke.py` verifies
actual CLI processes against a temporary authenticated Relay, pending-message
preservation, full local file hash parity, offline inspection and exclusion of
protocol secrets. UI acceptance and plugin integration remain separate.
