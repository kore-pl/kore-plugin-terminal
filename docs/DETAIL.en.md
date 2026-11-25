# kore-plugin-terminal — Web terminal plugin

## What it does

This is kore's web terminal plugin (id `terminal`). Every session is a **real PTY plus a login shell**,
not a command-execution wrapper: writing means keystrokes and reading means terminal output (escape
sequences included), ready for a front-end such as xterm.js. Since 0.2.0 it adds **method-level
capabilities**, **per-session persisted command history**, and **reconnection** (resumable read
cursors). Presentation-only concerns such as font size, full screen, themes and split panes are out
of scope for this repository.

- Protocol: `kore.plugin.v1`, `lifecycle=resident` (the terminal must stay alive), depth level `production`.
- Capabilities: `system:read` + `system:admin` + `proc:write`, with method-level `method_capabilities`.
- Resource budget: 12 MiB memory / 25% CPU / 64 pids.

## PTY implementation (no third-party libraries)

`internal/terminal/pty_linux.go` uses only the standard library and follows the fixed Linux flow to
open a pseudo-terminal: open `/dev/ptmx`, unlock the slave (TIOCSPTLCK), get the slave number
(TIOCGPTN) and open `/dev/pts/<n>`, start a login shell with `Setsid` + `Setctty` (so the process
group id equals the pid, allowing `kill(-pgid)` to kill the whole tree on close), resize with
`TIOCSWINSZ`. The master fd is non-blocking and reads/writes use `select` so a single paste cannot
stall the loop. Platform scope: builds on `linux/amd64`, `linux/arm64`, `linux/riscv64`,
`linux/loong64`; other platforms explicitly return `E_UNSUPPORTED`.

## Protocol methods (kore.plugin.v1)

| Method | Input | Notes |
| --- | --- | --- |
| `kore.ping` | `{}` | Liveness and runtime metrics |
| `kore.shutdown` | `{}` | Flush history, close all sessions, graceful exit |
| `terminal.create` | `{client_id?,cols?,rows?,shell?}` | Defaults 80x24, `/bin/bash`, `default` |
| `terminal.write` | `{session_id,client_id?,data}` | `data` is base64; input is recorded in history |
| `terminal.resize` | `{session_id,client_id?,cols,rows}` | Echoes after `TIOCSWINSZ` takes effect |
| `terminal.read` | `{session_id,client_id?,cursor?,max_bytes?}` | Incremental read; with `cursor` resumes from it |
| `terminal.attach` | `{session_id,client_id?,cursor?}` | Reconnect a live session, verify cursor is resumable |
| `terminal.close` | `{session_id,client_id?}` | `kill(-pgid)` + close fd + remove from map |
| `terminal.list` | `{client_id?}` | Without it: admin view (all); with it: filter by owner |
| `terminal.history` | `{session_id?,client_id?,limit?}` | Query command history (newest first) |
| `terminal.history.clear` | `{session_id?,client_id?}` | Clear the caller's history |

**Data encoding**: `terminal.write.data`, `terminal.read.data` and `terminal.history.entries[].data`
all use standard base64 (with `=` padding) — terminal output may contain arbitrary binary data and
multi-byte UTF-8 sequences split across frames; putting it straight into JSON would silently replace
them with U+FFFD. The front-end decodes once and hands the bytes to `term.write(Uint8Array)`.
Invalid base64 returns `E_INVALID`.

## Session ownership and unauthorized rejection

`terminal.create` may carry a `client_id` (letters/digits/`-_.:@`, <=64 bytes, default `default`);
ownership is immutable after creation. `write/read/resize/close/attach/history` must carry the same
`client_id` or they return `E_UNAUTHORIZED` without touching the owner's session. **Trust model**:
`client_id` is a logical ownership label inside the plugin, not an authentication credential; the
real authentication boundary is the kernel's `method_capabilities` authorization.

## Reconnection and read cursors

Each session keeps a monotonically increasing absolute byte cursor `offset` (bytes delivered),
advanced only by `read`. `cursor == offset` is an incremental read; `cursor < offset` within the
replay window replays bytes `[cursor, offset)` (`replay=true`); earlier than the window's lower bound
returns `E_CONFLICT`, ahead returns `E_INVALID`. The replay window `KORE_TERMINAL_REPLAY_BYTES`
defaults to 64 KiB (max 4 MiB) and keeps only the most recently delivered bytes; repeated reads of the
same cursor are idempotent. A session lives until explicit close, shell exit or idle reclamation
(default 1800s, lazily driven by calls, no ticker); after reclamation it cannot be resumed — `attach`
returns `E_NOTFOUND`.

## Command history

Only `terminal.write` input is recorded, grouped by session, stored at
`<KORE_STATE_DIR>/history/session-<id>.json` (directory 0700, file 0600, atomic write). Per-session
entry count (default 200), per-entry bytes (default 4096) and retained sessions (default 50) are all
configurable; excess entries are evicted oldest-first, oversized entries are truncated and marked
`truncated`. Persistence is best-effort: failures do not reject terminal input and the error is
honestly returned via `terminal.history.storage_error`. With `KORE_TERMINAL_HISTORY=off` or a missing
`KORE_STATE_DIR`, history is disabled with a WARN. **Privacy**: history may contain passwords typed
by the user; the plugin never writes input to logs, but pasting plaintext passwords into the terminal
is still discouraged.

## Security and resources

Session cap defaults to 4 (`KORE_TERMINAL_MAX_SESSIONS`, 1-16, over-limit returns `E_RATELIMIT`);
single read/write 1 MiB, 5s budget per request; `shell` must be an absolute path to an existing
executable; the child shell's environment strips `KORE_PLUGIN_TOKEN` so protocol credentials do not
leak into the terminal.

Environment variables: `KORE_TERMINAL_MAX_SESSIONS` (4), `KORE_TERMINAL_IDLE_SEC` (1800),
`KORE_TERMINAL_SHELL` (`/bin/bash`), `KORE_TERMINAL_REPLAY_BYTES` (65536),
`KORE_TERMINAL_HISTORY` (on), `KORE_TERMINAL_HISTORY_MAX` (200),
`KORE_TERMINAL_HISTORY_ENTRY_BYTES` (4096), `KORE_TERMINAL_HISTORY_SESSIONS` (50).

## Output channel: polling, not events

The conclusion is to poll `terminal.read` (incremental read) plus `attach`/`cursor` for recovery, not
event push: the protocol has no registered `terminal.output` event and the kernel does not forward
plugin events to SSE. A front-end can poll every 30-80ms or read immediately after input.

## Installation and build

A standalone Go module depending only on `kore/pkg/pluginapi` and the standard library; no third-party
dependencies and no committed `go.work`. Build baseline:
`CGO_ENABLED=0 GOWORK=off GOAMD64=v1 -trimpath -buildvcs=false -ldflags="-s -w"`.
Local verification: `gofmt -l .`, `GOWORK=off go vet ./...`,
`GOWORK=off go test -count=1 -timeout 200s ./...`, `make dist` (bit-for-bit reproducible packaging).
Release artifacts: `dist/terminal-<version>.tar.gz` + `.sha256`. Do not open a public issue for
vulnerabilities — disclose privately per [SECURITY.md](https://github.com/kore-pl/kore-plugin-terminal/blob/main/SECURITY.md).
