# jand

English · [简体中文](README.md) · [Landing Page](docs/index.html) · [Real Collaboration Replay (Sanitized)](docs/demo.html)

**Stop being the copy-paste middleman between two AI agents.**

jand is an open-source, end-to-end encrypted channel for AI agents to hand off tasks and collaborate across machines (**v0.5.1**). Share a one-time link so your agent and your colleague's agent can align specs, SQL, schemas, or debugging evidence directly against their own local context — while each human stays strictly in control of decisions.

- **Not MCP / remote control.** Your code, data, and secrets stay on your machine. Received files, messages, goals, and workflow rules are always treated as untrusted input; the local user authorizes every key step.
- **Hand off or collaborate.** Send an encrypted brief (≤10 MiB, 30 min in-memory relay) or add `--chat` to invite the other agent into a goal-scoped conversation with message budgets, request tracking, delivery superseding, and human checkpoints.
- **Zero-dependency Go binary.** Works out of the box with the public relay `https://jand.zhou-wei.com` (anonymous invites supported) or [your own self-hosted relay](deploy/INSTALL.md). Also available via DSH plugin (`dsh-jand`) and WorkBuddy skill.

## Install

Run this in a terminal on macOS or Linux.

```bash
curl -fsSL https://raw.githubusercontent.com/jaredchao/jand/main/install.sh | sh
```

Run this in PowerShell on Windows.

```powershell
irm https://raw.githubusercontent.com/jaredchao/jand/main/install.ps1 | iex
```

The installer downloads the package for your platform from [GitHub Releases](https://github.com/jaredchao/jand/releases), verifies its SHA-256 checksum, and starts `jand setup`. Setup configures the relay and receive directory, can add instructions for Claude Code or Codex, and checks the connection by sending a file to itself. Run `jand setup` again to change those settings.

Want to see how a two-agent collaboration feels by yourself first? Run `jand demo` to launch a 30-second offline sandbox (local ephemeral relay + two simulated agents + one human decision + auto-opened chat view page).

## Send your first task

On the sender's machine, put the task brief in `task.md` and run the following command.

```bash
jand send task.md
```

Share the full receive link privately. On the recipient's machine, replace `<receive-link>` with that link.

```bash
jand --out ./inbox '<receive-link>'
```

The `queued` status confirms that the relay accepted the encrypted file. To wait for a verified receipt that the recipient saved it, send with `jand send --wait 10m task.md` and look for `delivered`.

## Start a collaboration

Add `--chat` and a goal that tells both agents what to deliver.

```bash
jand send --chat --goal 'Agree on the response fields for GET /users' task.md
```

Share the receive link and keep the sender's chat ID for local commands. The recipient claims the file as above and reviews the invitation. With their user's approval, they join using the same receive link.

```bash
jand chat join '<receive-link>'
```

Run `jand help agent` before using jand from an agent. It prints the operating rules and can show the guide for each role with `jand help agent chat-host` or `jand help agent chat-guest`. Use `jand chat list` to find local conversations and `jand chat log <chat>` to review their history.

## Build from source

Building requires Go 1.24 or later. jand uses the Go standard library, and the built binary runs without a Go installation.

```bash
go build -o bin/jand ./cmd/jand
./bin/jand --help
```

## More documentation

Most detailed guides are in Chinese.

- [Agent operating guide](docs/AGENT.md) and [task handoff template](docs/jand-template.md)
- [File handoff protocol](docs/HTTP_DESIGN.md) and [collaboration protocol](docs/CHAT_DESIGN.md)
- [Relay deployment guide](deploy/INSTALL.md) and [configuration reference](docs/CONFIG_DESIGN.md)
- [MIT license](LICENSE)
