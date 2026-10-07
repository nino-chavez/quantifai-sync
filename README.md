# quantifai-sync

Telemetry sync agent for QuantifAI. Reads local Claude Code session data, parses token counts
and cost, and streams usage to a QuantifAI dashboard.

> **Status: paused.** QuantifAI stopped active development in 2026. `v0.1.0` is real, tagged,
> and installable, and the code is kept public because the scrubbing model below is the part
> worth reading. Nothing here is being maintained — treat it as reference, not a dependency.

## Install

```bash
brew tap nino-chavez/quantifai https://github.com/nino-chavez/quantifai-homebrew-tap
brew install quantifai-sync
```

The explicit URL is required: Homebrew's short `brew tap user/name` form looks for a repo
called `homebrew-name`, and the tap lives at `quantifai-homebrew-tap`.

Or grab a binary from [releases](https://github.com/nino-chavez/quantifai-sync/releases)
— darwin and linux, arm64 and amd64, and windows amd64.

```bash
quantifai-sync install      # set up the background service
quantifai-sync healthcheck  # verify it's reading and sending
quantifai-sync uninstall
```

On Windows, `install` registers a Task Scheduler task that starts the agent at logon, as the
user who ran `install`. Task Scheduler only lets administrators register tasks, so run `install`
from an elevated prompt signed in as that user; it prints the account it registered.

## What it does

Watches session files, parses them into usage records, and sends them. Along the way it
classifies intent, resolves identity, and applies pricing so cost is computed locally rather
than inferred server-side.

Runs as a background service with a menu-bar tray, self-updates, and carries its own health
check so a silently-dead agent is detectable rather than merely quiet.

## The part worth reading: Lite mode scrubs before sending

An ingest key prefixed `ql_` puts the agent in Lite mode, and Lite mode strips PII from every
record *before* transmission — not at the server, on your machine.

**Stripped:** prompt and response content, content length, file paths, git name, git email, OS
username, machine ID, working directory, git branch, remote URL.

**Preserved:** message ID, session ID, timestamp, model, tokens, cost, the last path segment
of the project, record type, tool names, intent tag.

The list is explicit in `internal/parser/scrub.go` rather than described in a privacy policy,
which is the only version of this claim that can be checked. Reading a telemetry agent's
scrub function is a reasonable thing to want to do before you run it; this one is 75 lines.

## Build

```bash
go build ./...   # Go 1.22
go test ./...
```

## License

The Homebrew formula declares MIT. No `LICENSE` file is committed, so the default —
all rights reserved — is what actually applies until one lands.
