# PeekAStokk 👀

Live log viewer in the browser. Point it at one or more log files and watch
them stream in a single web UI — with filtering, per-file toggles, pause, and
error/warning highlighting.

Written in Go with almost no dependencies — the standard library plus
`golang.org/x/crypto` (Argon2id password hashing for the optional auth).

## Why PeekAStokk?

As a DevOps engineer working across many companies, developers, and dev
servers, I kept needing a quick way for developers to check application
logs — but existing log viewers were either built for production (complex,
heavy, or costly) or just not made for dev-server simplicity. So I built
PeekAStokk: simple, lightweight, and easy for developers to use.

## Quick start

```sh
go build -o peekastokk .
./peekastokk /var/log/app.log /var/log/nginx/access.log
./peekastokk /var/log/myapp/            # a directory: every file inside it
./peekastokk '/var/log/*.log'           # glob patterns (quote them so the
./peekastokk '/var/log/myproject*'      #  shell passes them through)
# open http://127.0.0.1:8844/
```

Directories expand to the files directly inside them (dot-files and
subdirectories are skipped); glob patterns expand at startup. A plain path
that doesn't exist yet is waited for, but an unmatched pattern or empty
directory is a startup error.

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
  Deselecting a file purges it from browser memory again. The selection is
  remembered per browser and restored on reload.
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
| `-auth`           | (off)            | Require HTTP basic auth, `user:password`; empty disables        |
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

## Authentication

By default the UI is open (no auth) — fine for localhost. To require a
username and password, generate an Argon2id hash and put it in the config:

```sh
peekastokk -hash-password       # prompts, prints $argon2id$v=19$m=65536,...
```

```ini
auth = dev:$argon2id$v=19$m=65536,t=3,p=4$...   ; config file (preferred)
```

A plaintext password also works (`auth = dev:s3cret`, or
`-auth dev:s3cret` — visible in process lists) but logs a warning at
startup nudging you toward the hash.

The browser prompts once and everything — UI, live stream, scrollback — is
protected; `/healthz` stays open for load-balancer probes. `-auth ""` on
the command line overrides the config back to open access. Credentials are
compared in constant time, the slow hash verification is cached per process
and serialized (no CPU-burn from brute-force floods), and rejected attempts
are logged.

## Security note

PeekAStokk serves whatever it tails. It binds to `127.0.0.1` by default;
if you expose it on another interface with `-addr`, enable `auth` — and
since basic auth is sent as-is over plain HTTP, put it behind a reverse
proxy that handles TLS.

Absolute file paths never leave the server: clients (and anything
inspecting the traffic) only ever see opaque file ids and base names
(`app.log`, deduplicated as `app.log #2`), in API responses, the SSE
stream, and query parameters alike — so the host's directory layout is
not disclosed.

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

## License

[GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0). In short: you
may use, modify, and redistribute this software freely, but if you run a
modified version as a network service, you must offer its source to the
users of that service.
