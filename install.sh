#!/bin/sh
# PeekAStokk installer: Linux only, downloads the right release binary for
# this machine's CPU, installs it (with its man page) to /usr/local, creates
# an unprivileged system service account, and — when systemd is the running
# init system — installs and enables a hardened systemd unit. On any other
# init system only the binary and man page are installed; wire the service
# up to your init/supervisor of choice yourself.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/shellsecrets/peekastokk/main/install.sh | sudo sh
#
# Updating: re-run the exact same command. An existing installation is
# detected and upgraded in place — the binary and man page are replaced
# (atomically, so a running service is unaffected until restart), while
# the config, service account, and systemd unit are left untouched — and
# the service, if running, is restarted onto the new binary. When the
# installed version already matches the requested one, nothing is changed.
#
# Environment overrides:
#   PEEKASTOKK_REPO     "owner/repo" on GitHub (default: shellsecrets/peekastokk)
#   PEEKASTOKK_VERSION  a release tag to install instead of the latest (e.g. v1.1)
#   PEEKASTOKK_USER     service account name (default: peekastokk)
#   PEEKASTOKK_BINDIR   where to install the binary (default: /usr/local/bin)
#   PEEKASTOKK_MANDIR   where to install the man page (default: /usr/local/share/man/man1)
#   PEEKASTOKK_DOCDIR   where to install README/LICENSE/config.example (default: /usr/local/share/doc/peekastokk)
#   PEEKASTOKK_FORCE    set to 1 to reinstall even when the version already matches
#
# This script is written for POSIX sh (no bashisms: no arrays, no `[[ ]]`,
# no `local`) so it runs the same under dash, ash/busybox, and bash.

set -eu

REPO="${PEEKASTOKK_REPO:-shellsecrets/peekastokk}"
VERSION="${PEEKASTOKK_VERSION:-latest}"
SERVICE_USER="${PEEKASTOKK_USER:-peekastokk}"
BINDIR="${PEEKASTOKK_BINDIR:-/usr/local/bin}"
BIN_PATH="$BINDIR/peekastokk"
MANDIR="${PEEKASTOKK_MANDIR:-/usr/local/share/man/man1}"
DOCDIR="${PEEKASTOKK_DOCDIR:-/usr/local/share/doc/peekastokk}"
CONFIG_DIR="/etc/peekastokk"
UNIT_PATH="/etc/systemd/system/peekastokk.service"

info() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
err() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# --- sanity checks ----------------------------------------------------

[ "$(uname -s)" = "Linux" ] || err "PeekAStokk releases are Linux-only (detected $(uname -s))."

if [ "$(id -u)" -ne 0 ]; then
	err "this installer needs root (creates a system user, writes to $CONFIG_DIR, $BINDIR, and /etc/systemd/system). Re-run with sudo."
fi

if have curl; then
	fetch() { curl -fsSL "$1" -o "$2"; }
elif have wget; then
	fetch() { wget -q -O "$2" "$1"; }
else
	err "need curl or wget to download the release."
fi

# --- detect CPU family and word size separately, then map to a GOARCH -

family=""
case "$(uname -m)" in
	x86_64 | amd64 | i386 | i486 | i586 | i686)
		family=x86
		;;
	aarch64 | aarch64_be | arm64 | armv6l | armv7l | armv7 | armv8l | arm)
		family=arm
		;;
	*)
		err "unsupported CPU architecture: $(uname -m)" ;;
esac

# getconf LONG_BIT reflects the running userland's word size, which is the
# thing that actually needs to match the binary — uname -m alone can be
# wrong on a 64-bit kernel running a 32-bit userland (common on some
# Raspberry Pi setups). Fall back to uname -m if getconf is unavailable.
bits="$(getconf LONG_BIT 2>/dev/null || true)"
if [ -z "$bits" ]; then
	case "$(uname -m)" in
		x86_64 | aarch64 | aarch64_be | arm64 | armv8l) bits=64 ;;
		*) bits=32 ;;
	esac
fi

if [ "$family" = "x86" ]; then
	if [ "$bits" = "64" ]; then goarch=amd64; else goarch=386; fi
else
	if [ "$bits" = "64" ]; then goarch=arm64; else goarch=arm; fi
fi
info "detected linux/$goarch (family=$family, bits=$bits)"

# --- download and verify ------------------------------------------------

if [ "$VERSION" = "latest" ]; then
	base_url="https://github.com/$REPO/releases/latest/download"
else
	base_url="https://github.com/$REPO/releases/download/$VERSION"
fi
asset="peekastokk_linux_${goarch}.tar.gz"

workdir="$(mktemp -d 2>/dev/null || echo "/tmp/peekastokk-install.$$")"
mkdir -p "$workdir"
trap 'rm -rf "$workdir"' EXIT

info "downloading $asset ($VERSION) from $REPO"
fetch "$base_url/$asset" "$workdir/$asset"
fetch "$base_url/checksums.txt" "$workdir/checksums.txt" || warn "could not fetch checksums.txt; skipping verification"

if [ -f "$workdir/checksums.txt" ]; then
	sum_line="$(grep " $asset\$" "$workdir/checksums.txt" || true)"
	[ -n "$sum_line" ] || err "checksums.txt has no entry for $asset"
	expected="$(printf '%s' "$sum_line" | cut -d' ' -f1)"
	if have sha256sum; then
		actual="$(sha256sum "$workdir/$asset" | cut -d' ' -f1)"
	elif have shasum; then
		actual="$(shasum -a 256 "$workdir/$asset" | cut -d' ' -f1)"
	elif have openssl; then
		actual="$(openssl dgst -sha256 "$workdir/$asset" | sed 's/^.* //')"
	else
		warn "no sha256sum/shasum/openssl found; skipping checksum verification"
		actual="$expected"
	fi
	[ "$actual" = "$expected" ] || err "checksum mismatch for $asset (expected $expected, got $actual)"
	info "checksum verified"
fi

tar -xzf "$workdir/$asset" -C "$workdir"
[ -f "$workdir/peekastokk" ] || err "downloaded archive did not contain a peekastokk binary"
chmod 0755 "$workdir/peekastokk"

# --- update detection ------------------------------------------------------

# An existing binary means this run is an update: same steps, but the
# service is restarted onto the new binary at the end, and a matching
# version short-circuits into a no-op (unless PEEKASTOKK_FORCE=1).
updating=0
old_version=""
if [ -e "$BIN_PATH" ]; then
	updating=1
	old_version="$("$BIN_PATH" -version 2>/dev/null || echo unknown)"
fi
new_version="$("$workdir/peekastokk" -version 2>/dev/null || echo unknown)"

if [ "$updating" = "1" ] && [ "${PEEKASTOKK_FORCE:-0}" != "1" ] \
	&& [ "$new_version" != "unknown" ] && [ "$new_version" = "$old_version" ]; then
	info "already up to date ($old_version); nothing to do. Set PEEKASTOKK_FORCE=1 to reinstall anyway."
	exit 0
fi

# --- install the binary --------------------------------------------------

# Install via a temp name + rename: the swap is atomic, and a running
# service keeps executing the old inode (no ETXTBSY, no truncation race)
# until it is restarted below.
mkdir -p "$BINDIR"
install -m 0755 "$workdir/peekastokk" "$BIN_PATH.new"
mv -f "$BIN_PATH.new" "$BIN_PATH"
if [ "$updating" = "1" ]; then
	info "updated $BIN_PATH ($old_version -> $new_version)"
else
	info "installed $BIN_PATH ($new_version)"
fi

# --- install the man page -------------------------------------------------

if [ -f "$workdir/peekastokk.1" ]; then
	mkdir -p "$MANDIR"
	install -m 0644 "$workdir/peekastokk.1" "$MANDIR/peekastokk.1"
	# Refresh the whatis/apropos database if this system keeps one; harmless
	# to skip (man still finds the page by path either way) when it doesn't.
	if have mandb; then
		mandb -q >/dev/null 2>&1 || true
	elif have makewhatis; then
		makewhatis "$MANDIR" >/dev/null 2>&1 || true
	fi
	info "installed man page: $MANDIR/peekastokk.1 (try: man peekastokk)"
else
	warn "release archive did not include a man page; skipping"
fi

# --- docs: README, LICENSE, example config --------------------------------

# Plain overwrites, so an upgrade refreshes them alongside the binary
# (the same-version early exit above means they only ever lag when the
# release itself didn't change).
mkdir -p "$DOCDIR"
docs_installed=0
for doc in "$workdir"/README* "$workdir"/LICENSE* "$workdir/config.example"; do
	[ -f "$doc" ] || continue
	install -m 0644 "$doc" "$DOCDIR/$(basename "$doc")"
	docs_installed=$((docs_installed + 1))
done
if [ "$docs_installed" -gt 0 ]; then
	info "installed docs to $DOCDIR"
else
	warn "release archive included no README/LICENSE/config.example; skipping docs"
fi

# --- unprivileged service account -----------------------------------------

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
	info "creating system user '$SERVICE_USER' (no login, no home)"
	if have useradd; then
		useradd --system --no-create-home --shell /usr/sbin/nologin --user-group "$SERVICE_USER" \
			|| useradd --system --no-create-home --shell /sbin/nologin --user-group "$SERVICE_USER"
	elif have adduser; then
		# Alpine/BusyBox adduser.
		addgroup -S "$SERVICE_USER" 2>/dev/null || true
		adduser -S -D -H -s /sbin/nologin -G "$SERVICE_USER" "$SERVICE_USER"
	else
		err "neither useradd nor adduser found; create the '$SERVICE_USER' system user yourself and re-run."
	fi
else
	info "system user '$SERVICE_USER' already exists"
fi

# 'adm' is the traditional Debian/Ubuntu group with read access to
# /var/log; harmless to add, and a no-op where the group doesn't exist.
# It will NOT be enough for every log location — see the README section
# on granting log access (ACLs are the general-purpose answer).
if getent group adm >/dev/null 2>&1 && have usermod; then
	usermod -aG adm "$SERVICE_USER" 2>/dev/null || true
fi

# --- config directory ------------------------------------------------------

mkdir -p "$CONFIG_DIR"
if [ -f "$workdir/config.example" ]; then
	# Always refreshed on (re)install, unlike config itself: a pristine
	# reference copy that survives you editing the live config.
	cp "$workdir/config.example" "$CONFIG_DIR/config.example"
fi
if [ ! -e "$CONFIG_DIR/config" ]; then
	if [ -f "$workdir/config.example" ]; then
		cp "$workdir/config.example" "$CONFIG_DIR/config"
	else
		: >"$CONFIG_DIR/config"
	fi
	info "wrote a starter config to $CONFIG_DIR/config (edit it — 'file =' needs at least one log path)"
fi
# The config may later hold an auth password hash; keep it group-private.
chown -R "root:$SERVICE_USER" "$CONFIG_DIR"
chmod 0750 "$CONFIG_DIR"
chmod 0640 "$CONFIG_DIR/config"
[ -f "$CONFIG_DIR/config.example" ] && chmod 0640 "$CONFIG_DIR/config.example"

# --- systemd unit, only when systemd is the running init -------------------

if [ -d /run/systemd/system ] && have systemctl; then
	if [ -f "$UNIT_PATH" ]; then
		# Never rewrite an existing unit: it may carry operator edits
		# (ports, extra mounts, loosened hardening). Delete it and re-run
		# to regenerate a pristine one.
		info "existing $UNIT_PATH kept as-is"
	else
		info "systemd detected; installing $UNIT_PATH"
		cat >"$UNIT_PATH" <<EOF
[Unit]
Description=PeekAStokk log viewer
Documentation=https://github.com/$REPO
After=network.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
# No config env var needed: the app checks $CONFIG_DIR/config on its own
# as an unconditional last resort, after any user-specific config.
ExecStart=$BIN_PATH
Restart=on-failure
RestartSec=2

# Hardening. Filesystem reads are deliberately NOT restricted beyond normal
# permissions/ACLs, since tailed log paths vary by deployment; only writes
# and privilege escalation are locked down (this service only ever reads).
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=read-only
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectKernelLogs=true
ProtectControlGroups=true
ProtectClock=true
ProtectHostname=true
RestrictSUIDSGID=true
RestrictRealtime=true
RestrictNamespaces=true
LockPersonality=true
RemoveIPC=true
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

[Install]
WantedBy=multi-user.target
EOF
		systemctl daemon-reload
		info "unit installed but not started. Next steps:"
		printf '\n'
		printf '  1. edit %s and add "file = ..." for each log to tail\n' "$CONFIG_DIR/config"
		printf "  2. make sure '%s' can read those files — group membership won't cover most\n" "$SERVICE_USER"
		printf '     app-owned logs; grant access with (repeat per log directory):\n'
		printf '         setfacl -R -m u:%s:rX /path/to/logs\n' "$SERVICE_USER"
		printf '         setfacl -R -d -m u:%s:rX /path/to/logs   # applies to future files too\n' "$SERVICE_USER"
		printf '  3. sudo systemctl enable --now peekastokk\n'
		printf '  4. journalctl -u peekastokk -f    # "permission denied" here means step 2 is incomplete\n'
		printf '\n'
	fi
	# On an update, move a running service onto the new binary; a stopped
	# or not-yet-enabled service is left alone.
	if [ "$updating" = "1" ] && systemctl is-active --quiet peekastokk 2>/dev/null; then
		info "restarting running peekastokk service onto the new binary"
		systemctl restart peekastokk \
			|| warn "restart failed; check: journalctl -u peekastokk"
	fi
else
	if [ "$updating" = "1" ]; then
		warn "restart your peekastokk service yourself so it picks up the new binary."
	fi
	warn "systemd not detected as the running init; only the binary was installed."
	warn "wire up $BIN_PATH to your init system (OpenRC, runit, SysV, ...) yourself, running it as '$SERVICE_USER'; it finds $CONFIG_DIR/config on its own, no extra environment needed."
	warn "whatever init you use, '$SERVICE_USER' will also need read access granted to any configured log paths (see the README)."
fi

info "done. binary: $BIN_PATH  man: $MANDIR/peekastokk.1  docs: $DOCDIR  config: $CONFIG_DIR/config  user: $SERVICE_USER"
