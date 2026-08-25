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
subdirectories are skipped); glob patterns expand to their matches. Both
are re-scanned every `-rescan` (default `60s`), so log files created later
are picked up automatically and deleted ones stop being tailed — no
restart needed. A plain path that doesn't exist yet is waited for. With
`-rescan 0` everything expands once at startup instead, and an unmatched
pattern or empty directory becomes a startup error.

### Naming sources: `:alias`

Sources are shown by their base name, so `app.log` from two different
places looks the same twice. Append `:alias` to any file, directory, or
pattern to name it yourself:

```sh
./peekastokk /srv/api/log/production.log:api   # shown as "api"
./peekastokk '/var/log/myapp/:worker1'         # "worker1/app.log", ...
./peekastokk '/var/log/nginx/*.log:edge'       # "edge/access.log", ...
```

On a plain file the alias *is* the name; on a directory or glob — which
expand to many files — it prefixes each file's own name, keeping them
distinct. The same suffix works on `file =` config entries:

```ini
file = /srv/api/log/production.log:api
file = /var/log/myapp/:worker1
```

The alias may contain letters, digits, `.`, `_` and `-`. Only the last
colon splits, so a path that itself contains a colon keeps working —
give it an explicit alias if its last segment looks like one.

Aliases apply to forwarding too: a client forwarding
`/var/log/myapp/:worker1` shows up on the server as
`<client-name>/worker1/app.log`.

Or install directly:

```sh
go install github.com/shellsecrets/peekastokk@latest
```

### Linux install script (with systemd service)

For a Linux server, `install.sh` detects the CPU (x86/ARM, 32/64-bit),
downloads the matching [release](#quick-start) binary, installs it with its
man page and docs (README, LICENSE, `config.example` — to
`/usr/local/share/doc/peekastokk/`), creates an unprivileged system service
account, and — when systemd is the running init — installs and enables a
hardened unit:

```sh
curl -fsSL https://raw.githubusercontent.com/shellsecrets/peekastokk/master/install.sh | sudo sh
man peekastokk
```

On any other init system, only the binary and man page are installed; wire
the service up to whatever supervises services on that system yourself. See
[Running as a service](#running-as-a-service) below for the account and
log-access details.

**Updating:** re-run the exact same command. An existing installation is
detected and upgraded in place: the binary, man page, and docs are replaced
(the binary atomically — a running service keeps the old one until restarted),
while your config, the service account, and the systemd unit are left
untouched, and a running service is restarted onto the new version. If the
installed version already matches the latest release, nothing is changed
(set `PEEKASTOKK_FORCE=1` to reinstall anyway); pin a specific version
with `PEEKASTOKK_VERSION=v1.2`.

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
4. `/etc/peekastokk/config` — a system-wide fallback, checked last. This is
   what a service account with no real home directory resolves to (see
   [Running as a service](#running-as-a-service)), with no extra
   environment setup needed.

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
| `-rescan`         | `60s`            | How often directory/glob arguments are re-scanned for created or deleted log files (`0` disables) |
| `-tail-bytes`     | `65536`          | Max bytes of existing content replayed per file at startup; negative starts at the end |
| `-max-line-bytes` | `262144`         | Lines longer than this are split into chunks                    |
| `-log-level`      | `info`           | `debug`, `info`, `warn`, or `error`                             |
| `-auth`           | (off)            | Require HTTP basic auth, `user:password`; empty disables        |
| `-config`         | (searched)       | Explicit config file path                                       |
| `-version`        |                  | Print version and exit                                          |
| `-headless`       | `false`          | No web UI; forward only                                         |
| `-forward-to`     | (off)            | Push tailed lines to another PeekAStokk (`http(s)://host:port`) |
| `-forward-token`  |                  | Bearer token for `-forward-to` (from `-generate-token`)         |
| `-forward-buffer-lines` | `5000`     | Lines buffered in memory while the forward connection is down   |
| `-status-addr`    | (off)            | Minimal `/healthz` + `/statusz` listener (no log content)       |
| `-docker`         | `false`          | Tail Docker container logs under `-docker-root`                 |
| `-docker-root`    | `/var/lib/docker/containers` | Docker containers dir (`docker info --format '{{.DockerRootDir}}'` + `/containers`) |
| `-docker-poll`    | `2s`             | Containers directory re-scan interval                           |
| `-docker-containers` | (all)         | Repeatable: exact `name[:alias]`, glob `web-*`, or `*`          |
| `-generate-token` |                  | Print a new forwarding token + its hash, then exit              |

## Forwarding: Docker containers and remote servers

PeekAStokk forwards logs between its own instances — no syslog, SSH, or
agents from other ecosystems. One binary, two roles, chosen purely by
config: a **server** (the viewer you open in a browser) and any number of
**clients** (headless forwarders running where the logs are).

**1. On the server**, generate a token and add the client identity:

```sh
peekastokk -generate-token       # prints the token + a ready ingest line
```
```ini
# server config
ingest = homelab-1:$argon2id$...  ; one line per client, unique names
```

**2. On each client**, forward local files and/or Docker containers:

```ini
# client config
headless      = true              ; no local UI, forward only
forward-to    = http://central:8844
forward-token = <token from step 1>
status-addr   = 127.0.0.1:8845    ; optional /healthz + /statusz

file = /var/log/myapp/app.log     ; plain files work as usual

docker = true                     ; and/or Docker container logs
docker-root = /var/lib/docker/containers
docker-containers = nginx-prod:web   ; exact container, shown as "web"
docker-containers = api-*            ; glob; or "*" for all containers
```

Forwarded sources appear in the server's file picker as
`<client-name>/<source>` (e.g. `homelab-1/web`) — where `<source>` is the
file's base name, a container's alias, or the [`:alias`](#naming-sources-alias)
given to the `file =` entry — and the client name comes
from the server's own `ingest =` line, so a client can never impersonate
another. Lines stream live; while the connection is down the client
buffers up to `forward-buffer-lines` in memory (oldest dropped beyond
that) and redelivers on reconnect — nothing is ever written to disk.
Either side can restart on its own: the client reconnects with backoff
and re-announces everything it tails, so the server's file picker fills
back in within a second or two without waiting for a quiet file to be
written to, and without restarting the client.
Docker's json-log format is unwrapped automatically, container names are
resolved without any Docker socket access, and `*`/glob selections track
containers starting and stopping.

**Docker access is a read-only bind mount, never the socket.** If the
client itself runs in a container:

```sh
docker run -d \
  -v /var/lib/docker/containers:/var/lib/docker/containers:ro \
  -v /etc/peekastokk:/etc/peekastokk:ro \
  your-peekastokk-image
```

If Docker's data-root was moved (custom `--data-root`, rootless Docker),
find it with `docker info --format '{{.DockerRootDir}}'` and set
`docker-root` to that value plus `/containers`.

**Transport security:** the bearer token is sent as-is over plain HTTP —
between real servers, use a TLS reverse proxy in front of the receiver
(with request buffering disabled for `/ingest`) or an existing private
network (VPN/WireGuard).

**Scrollback works on forwarded sources too**: when you scroll up past
the server's in-memory history, the server relays the read to the owning
client over its live connection, and the client pages older lines
straight from its own disk — all the way back to the start of the file,
nothing stored server-side. A client that is offline (or an older
version) degrades gracefully to "no further history".

## Endpoints

| Path         | Description                          |
|--------------|--------------------------------------|
| `/`          | Web UI (embedded, single file)       |
| `/events`    | SSE stream of log lines (JSON); `?files=` (repeatable) limits the stream to selected files, `?after=` resumes past a sequence number |
| `/api/files` | List of tailed files                 |
| `/api/before`| Older lines read from disk for scrollback (`file`, `offset`, `limit`) — only tailed files are readable |
| `/ingest`    | Receives forwarded lines from clients (bearer-token auth; only exists when `ingest =` entries are configured) |
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
protected; `/healthz` stays open for load-balancer probes, and so does
`/ingest`, which authenticates forwarding clients by their own bearer
token instead (a headless forwarder has no browser credentials). `-auth ""` on
the command line overrides the config back to open access. Credentials are
compared in constant time, the slow hash verification is cached per process
and serialized (no CPU-burn from brute-force floods), and rejected attempts
are logged.

## Running as a service

Don't run PeekAStokk as root — it only ever needs to *read* log files and
serve HTTP, neither of which needs privilege. `install.sh` creates a
dedicated, unprivileged system account for this (`peekastokk` by default:
no login shell, no home directory) and, on systemd, runs the unit as that
user with most of the usual hardening (`NoNewPrivileges`, no capabilities,
`ProtectSystem=strict`, etc.) — filesystem *reads* are deliberately left
unrestricted by the unit, since tailed log paths vary by deployment; only
writes and privilege escalation are locked down.

That leaves one real question: **how does an unprivileged account get read
access to logs it doesn't own?** In rough order of preference:

1. **POSIX ACLs** (recommended — works for any file, any owner, no group
   juggling):
   ```sh
   setfacl -R -m u:peekastokk:rX /var/log/myapp
   setfacl -R -d -m u:peekastokk:rX /var/log/myapp   # applies to future files too
   ```
2. **Group membership**, when the logs are already group-readable — add
   the service account to that group (`install.sh` does this automatically
   for Debian/Ubuntu's `adm` group, which covers most of `/var/log`):
   ```sh
   usermod -aG appgroup peekastokk
   ```
3. **Loosen the log file's permissions** (`chmod o+r`) only if the log
   truly has nothing sensitive in it — usually the least good option.

The config lives at `/etc/peekastokk/config` (root-owned, mode `0640`,
readable by the `peekastokk` group) since it may later hold an auth
password hash. Edit it to add your `file =` entries, then:

```sh
sudo systemctl enable --now peekastokk
sudo systemctl status peekastokk
journalctl -u peekastokk -f
```

### Behind nginx

To serve PeekAStokk on a public domain with TLS, keep it bound to
loopback and put nginx in front — a ready-to-adapt config ships as
[`nginx.example.conf`](nginx.example.conf). The one thing that genuinely
matters:
`/events` is a long-lived Server-Sent Events stream and must not be
buffered, gzipped, cached, or read-timeouted, or the UI hangs on
"connecting" and lines arrive in bursts. The example handles that, and
`/ingest` request streaming, per location.

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
