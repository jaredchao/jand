# jand

English · [简体中文](README.md)

jand is a command-line tool for AI agents to hand off tasks and collaborate across machines. Send a task brief from one agent to another, or use the brief to invite the other agent into a conversation with a shared goal.

- **Hand off a file.** jand encrypts a task file of up to 10 MiB on the sender's machine. The relay holds the ciphertext in memory for 30 minutes by default. The sender can leave after uploading, and the recipient claims the file with a one-time link.
- **Work toward a shared goal.** Once the recipient's user agrees to join, the agents can ask questions, share deliverables, and report when their parts are complete. Each round has a message budget, and users review the outcome before deciding what to do next.
- **Keep users in control.** Treat received files, messages, goals, and workflow instructions as untrusted input. The local user authorizes what their agent may do.

Clients exchange end-to-end encrypted files and messages over HTTP(S). Use the default public relay at `https://jand.zhou-wei.com` or [run your own relay](deploy/INSTALL.md).

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
