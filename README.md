# PeekAStokk 👀

Live log viewer in the browser. Point it at one or more log files and watch
them stream in a single web UI — with filtering, per-file toggles, pause, and
error/warning highlighting.

Written in Go with **zero external dependencies** (standard library only).

## Quick start

```sh
go build -o peekastokk .
./peekastokk /var/log/app.log /var/log/nginx/access.log
# open http://127.0.0.1:8844/
```

Or install directly:

```sh
go install github.com/shellsecrets/peekastokk@latest
```

## Features

- **Live streaming** over Server-Sent Events — the browser reconnects
  automatically and resumes exactly where it left off (`Last-Event-ID`).
- **Rotation-safe tailing** — survives both `rename`+recreate and
  `copytruncate` style log rotation, and waits for files that don't exist yet.
- **History replay** — new browser tabs immediately see the most recent lines
  (2 000 by default).
- **Infinite scrollback, bounded memory** — scroll to the top and older lines
  page in straight from the file on disk (2 000 at a time), all the way back
  to the first line. Only a small window stays in the browser: the part you
  scrolled past is unloaded, and jumping back to the tail frees it all. The
  server keeps nothing extra in memory — the log file itself is the archive.
- **Multi-file, loaded lazily** — files are selected through a searchable
  dropdown (scales to hundreds of files; the trigger shows the current
  selection, e.g. `app.log +2`). Only the first file is streamed when the
  UI opens; the others cost nothing (no bandwidth, no browser memory — the
  stream itself is filtered server-side per client) until selected, which
  loads their recent lines straight from disk and joins their live stream.
  Deselecting a file purges it from browser memory again.
- **Filtering** — substring filter with match highlighting (`/` to focus,
  `Esc` to clear); error/warning lines are tinted automatically.
- **Pause, clear, follow** — the view sticks to the bottom until you scroll
  up; a "jump to latest" button brings you back.
- **Adjustable view size** — how many lines stay on screen (500 by default)
  is configurable via flag/config and editable live in the UI.
- **Bounded memory everywhere** — history ring buffer, capped line length,
  capped per-client queues (a slow client is evicted and reconnects, it can
  never stall tailing).

## Configuration file

Everything the flags cover — including the port and the list of files — can
live in a config file, so a bare `peekastokk` just works. The file is
searched in this order (first match wins), or given explicitly with
`-config <path>`:

1. `$XDG_CONFIG_HOME/peekastokk/config` (defaults to `~/.config/peekastokk/config`)
2. `~/.peekastokk` (a plain file)
3. `~/.peekastokk/config`

Format is simple `key = value`; keys match the flag names, plus `port` as a
shorthand and a repeatable `file`:

```ini
# ~/.config/peekastokk/config
port      = 9000          ; or: addr = 0.0.0.0:9000
history   = 5000
lines     = 500           ; lines kept on screen in the UI
poll      = 100ms
log-level = info

file = /var/log/app.log
file = ~/logs/worker.log  # ~ expands; repeat "file" per log
file = relative.log       # resolved against this config file's directory
```

Blank lines and `#`/`;` comments (including unquoted trailing comments) are
ignored; quote a value to keep a literal `#` or surrounding spaces. Unknown
or malformed keys fail fast at startup with a line number.

A ready-to-copy, fully commented example ships in the repo as
[`config.example`](config.example):

```sh
mkdir -p ~/.config/peekastokk
cp config.example ~/.config/peekastokk/config
```

**Precedence:** command-line flags beat the config file, which beats the
built-in defaults. Log files given as command-line arguments replace the
config file's `file` list entirely.

## Flags

| Flag              | Default          | Description                                                     |
|-------------------|------------------|-----------------------------------------------------------------|
| `-addr`           | `127.0.0.1:8844` | HTTP listen address                                             |
| `-history`        | `2000`           | Recent lines replayed to newly connected browsers               |
| `-lines`          | `500`            | Default lines kept on screen in the UI (adjustable there; a value chosen in the UI sticks per browser) |
| `-poll`           | `200ms`          | How often files are checked for new data                        |
| `-tail-bytes`     | `65536`          | Max bytes of existing content replayed per file at startup; negative starts at the end |
| `-max-line-bytes` | `262144`         | Lines longer than this are split into chunks                    |
| `-log-level`      | `info`           | `debug`, `info`, `warn`, or `error`                             |
| `-config`         | (searched)       | Explicit config file path                                       |
| `-version`        |                  | Print version and exit                                          |

## Endpoints

| Path         | Description                          |
|--------------|--------------------------------------|
| `/`          | Web UI (embedded, single file)       |
| `/events`    | SSE stream of log lines (JSON); `?files=` (repeatable) limits the stream to selected files, `?after=` resumes past a sequence number |
| `/api/files` | List of tailed files                 |
| `/api/before`| Older lines read from disk for scrollback (`file`, `offset`, `limit`) — only tailed files are readable |
| `/healthz`   | Health check                         |

## Security note

PeekAStokk serves whatever it tails, without authentication. It binds to
`127.0.0.1` by default; if you expose it on another interface with `-addr`,
put it behind a reverse proxy that handles auth/TLS.

## Architecture

```
tail.Tailer (one per file, polling; rotation/truncation aware)
     │  lines (channel, fan-in)
     ▼
hub.Hub (global sequence numbers, ring-buffer history, fan-out)
     │  per-subscriber buffered channels
     ▼
server (SSE /events with Last-Event-ID resume, embedded UI,
        /api/before reads older lines backwards from disk on demand)
```

Every streamed line carries its byte offset in its file; the UI uses the
oldest offset it holds as the anchor for backwards paging, so scrollback
pages are contiguous and never overlap the live view.

Polling (instead of inotify/kqueue) is deliberate: it needs no platform
code and works on filesystems that don't emit change events (NFS, SMB,
container mounts).

## Development

```sh
make test   # go test -race ./...
make vet
make build  # embeds the version via -ldflags
```
