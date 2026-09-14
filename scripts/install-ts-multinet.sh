#!/usr/bin/env bash
#
# install-ts-multinet.sh — install ts-multinet as a systemd service.
# Self-contained: builds (or accepts) the binary, installs the config and a
# unit, and starts the daemon. Browser login happens after install:
#
#   sudo scripts/install-ts-multinet.sh
#   sudo ${EDITOR:-nano} /etc/ts-multinet/config.json   # tailnets + resources
#   sudo systemctl restart ts-multinet
#   sudo ts-multinet login skynet                     # once per tailnet, forever
#   sudo ts-multinet select skynet nucbox             # expose peers (or the web UI)
#
# Host DNS is automatic on systemd-resolved hosts (per-TUN routing domains);
# without resolved, selected names still resolve via the /etc/hosts block.
# The web UI listens on http://127.0.0.1:8123 (config: ui_listen).
#
# No authkeys: the daemon uses persistent node state + browser login, so this
# script has no key handling. Existing config and state are never overwritten.
#
#   --config PATH    seed /etc/ts-multinet/config.json from PATH (default:
#                    keep existing, else cmd/ts-multinet/config.example.jsonc)
#   --binary PATH    install this binary instead of building from the clone
#   --uninstall      stop and remove binary, unit, and config
#   --purge          with --uninstall: also remove state (node keys, pins)
#
set -euo pipefail

BINDIR=/usr/local/bin
SYSCONF=/etc/ts-multinet/config.json
UNIT=/etc/systemd/system/ts-multinet.service
STATEDIR=/var/lib/ts-multinet

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SEED_CONFIG=""
BINARY=""
UNINSTALL=0
PURGE=0

while [ $# -gt 0 ]; do
    case "$1" in
    --config)
        SEED_CONFIG="${2:-}"
        shift 2
        ;;
    --binary)
        BINARY="${2:-}"
        shift 2
        ;;
    --uninstall)
        UNINSTALL=1
        shift
        ;;
    --purge)
        PURGE=1
        shift
        ;;
    -h | --help)
        grep '^#' "$0" | sed 's/^# \{0,1\}//'
        exit 0
        ;;
    *)
        echo "unknown option: $1 (see --help)" >&2
        exit 1
        ;;
    esac
done

if [ "$(id -u)" -ne 0 ]; then
    echo "run as root (sudo)" >&2
    exit 1
fi

uninstall() {
    systemctl disable --now ts-multinet 2>/dev/null || true
    rm -f "$UNIT" "$BINDIR/ts-multinet" "$SYSCONF"
    rmdir "$(dirname "$SYSCONF")" 2>/dev/null || true
    if [ "$PURGE" -eq 1 ]; then
        rm -rf "$STATEDIR"
        echo "removed state (node keys, selection pins) — tailnets must re-login"
    else
        echo "kept $STATEDIR (node identities); re-install logs in without a browser"
    fi
    systemctl daemon-reload
    echo "ts-multinet uninstalled"
    exit 0
}
[ "$UNINSTALL" -eq 1 ] && uninstall

# --- binary: explicit --binary, else build from the clone --------------------
SRC=""
if [ -n "$BINARY" ]; then
    [ -x "$BINARY" ] || {
        echo "--binary: $BINARY is not executable" >&2
        exit 1
    }
    SRC="$BINARY"
else
    CMD_DIR="$REPO_ROOT/cmd/ts-multinet"
    if [ ! -d "$CMD_DIR" ]; then
        echo "not run from a ts-plug clone and no --binary given." >&2
        echo "either clone https://github.com/th3wingman/ts-plug or pass --binary" >&2
        exit 1
    fi
    # go commonly installs to /usr/local/go/bin, which sudo's secure_path omits
    export PATH="$PATH:/usr/local/go/bin"
    echo "building ts-multinet from $REPO_ROOT ..."
    (cd "$REPO_ROOT" && make ts-multinet)
    SRC="$REPO_ROOT/build/ts-multinet"
    # the clone's build artifact belongs to the invoking user, not root
    [ -n "${SUDO_USER:-}" ] && chown "$SUDO_USER" "$SRC" 2>/dev/null || true
fi

install -m755 "$SRC" "$BINDIR/ts-multinet"

# --- config: keep existing, else seed ----------------------------------------
mkdir -p "$(dirname "$SYSCONF")"
if [ -f "$SYSCONF" ]; then
    echo "kept existing $SYSCONF"
elif [ -n "$SEED_CONFIG" ]; then
    [ -f "$SEED_CONFIG" ] || {
        echo "--config: $SEED_CONFIG not found" >&2
        exit 1
    }
    install -m600 "$SEED_CONFIG" "$SYSCONF"
    echo "installed $SYSCONF from $SEED_CONFIG"
else
    install -m600 "$REPO_ROOT/cmd/ts-multinet/config.example.jsonc" "$SYSCONF"
    echo "installed $SYSCONF from the example — edit it for your tailnets"
fi

# --- unit ---------------------------------------------------------------------
# StateDirectory matches the config default state_dir (/var/lib/ts-multinet),
# so node identities and selection pins survive restarts and reinstalls.
cat >"$UNIT" <<'EOF'
[Unit]
Description=ts-multinet — several tailnets on one host
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/ts-multinet -config /etc/ts-multinet/config.json
Restart=on-failure
StateDirectory=ts-multinet

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now ts-multinet

if ! command -v resolvectl >/dev/null 2>&1 || [ ! -d /run/systemd/resolve ]; then
    echo "note: systemd-resolved not detected — host DNS will be skipped;"
    echo "      selected names still resolve via the /etc/hosts block"
fi

echo
echo "installed:"
echo "  $BINDIR/ts-multinet"
echo "  $SYSCONF"
echo "  $UNIT  (enabled, running)"
echo
echo "next steps:"
echo "  1. sudo ${EDITOR:-nano} $SYSCONF            # tailnets, cidrs"
echo "  2. sudo systemctl restart ts-multinet"
echo "  3. sudo ts-multinet login <tailnet>        # once per tailnet — browser URL"
echo "  4. sudo ts-multinet select <tailnet> <peer>...   # expose peers (or: forget, allow-all, domain)"
echo "  5. http://127.0.0.1:8123                   # web UI: dashboard, selection, logins, reload"
echo "     sudo ts-multinet status                 # states + selections"
echo "     journalctl -u ts-multinet -f            # watch it come up"
