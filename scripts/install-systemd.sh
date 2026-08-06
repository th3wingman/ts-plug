#!/usr/bin/env bash
#
# install-systemd.sh — install ts-plug / ts-unplug / ts-router as systemd
# services. Self-contained: unit templates are embedded, so this file plus a
# binary is all a target host needs.
#
# Layout it manages per instance NAME:
#   /usr/local/bin/<tool>            binary
#   /etc/systemd/system/<tool>@.service   shared template unit
#   /etc/<tool>/NAME.env             TS_AUTHKEY + ARGS (mode 0600)
#   /var/lib/<tool>/NAME/            tsnet state: node keys, certs (systemd StateDirectory)
#   /etc/ts-router/NAME/routes.json  ts-router only
#
# Examples:
#   # Expose local sshd at my-laptop-ssh.<tailnet>.ts.net:22
#   sudo ./install-systemd.sh ts-plug --name my-laptop-ssh --port 22
#
#   # Expose local Grafana at https://grafana.<tailnet>.ts.net
#   sudo ./install-systemd.sh ts-plug --name grafana --proto https --dst-port 3000
#
#   # Bring tailnet postgres to 127.0.0.1:5432
#   sudo ./install-systemd.sh ts-unplug --name db --port 5432 --mode tcp db.tailnet.ts.net:5432
#
#   # ts-router instance from a routes.json
#   sudo ./install-systemd.sh ts-router --name skynet --config ./routes.json
#
#   # Remove (keeps node keys unless --purge)
#   sudo ./install-systemd.sh ts-plug --name my-laptop-ssh --uninstall
#
# Auth key resolution order: --authkey, --env-file, $TS_AUTHKEY, existing
# /etc/<tool>/NAME.env, interactive prompt. After the first successful join
# the node identity lives in /var/lib/<tool>/NAME and the key may be removed.
#
# Works standalone via curl too — no clone, no Go toolchain. The binary is
# then downloaded from GitHub releases and checksum-verified:
#
#   curl -fsSL https://raw.githubusercontent.com/th3wingman/ts-plug/main/scripts/install-systemd.sh \
#     | sudo bash -s -- ts-plug --name my-host-ssh --port 22 --authkey tskey-auth-...
#
# (Piped stdin means no interactive key prompt — pass --authkey or TS_AUTHKEY.
#  Download the script first if you prefer the hidden prompt.)

set -euo pipefail

BINDIR=/usr/local/bin
GH_REPO="${TS_PLUG_REPO:-th3wingman/ts-plug}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" 2>/dev/null && pwd || true)"
REPO_ROOT="${SCRIPT_DIR:+$(cd "$SCRIPT_DIR/.." 2>/dev/null && pwd || true)}"

usage() {
    cat <<'EOF'
usage: install-systemd.sh <ts-plug|ts-unplug|ts-router> --name NAME [options]

common options:
  --name NAME        instance name; also the default tailnet hostname (required)
  --hostname HN      tailnet hostname if different from --name
  --authkey KEY      tailscale auth key (tskey-auth-...)
  --env-file PATH    read TS_AUTHKEY from an existing env file
  --binary PATH      install this binary instead of building/looking one up
  --version VER      download this release (e.g. v0.1.0) from GitHub instead
                     of building; default when nothing local is found: latest
  --arch A           override arch for downloads: amd64 | arm64 | armv7
  --args 'RAW'       raw tool arguments; escape hatch, replaces generated args
  --group G          let the service join a supplementary group (e.g. docker,
                     to reach group-owned unix sockets); writes a unit drop-in
  --uninstall        stop, disable and remove the instance
  --purge            with --uninstall: also delete state (node keys!) and config
  --dry-run          print what would be done without touching anything

ts-plug options (expose 127.0.0.1 to the tailnet):
  --proto P          tcp | http | https | dns        (default: tcp)
  --port N           listen and target port           (tcp default: 22)
  --src-port N       tailnet-side listen port
  --dst-port N       local destination port
  --dst-socket PATH  forward to a local unix socket instead of a port
                     (e.g. /run/ssh-unix-local/socket, /var/run/docker.sock)
  --run 'CMD'        upstream command ts-plug should supervise
                     (default: /bin/sleep infinity — i.e. plain forwarding)
  --public           enable Tailscale Funnel (https only)

ts-unplug options (bring a tailnet service to 127.0.0.1):
  --port N           local listen port
  --src-socket PATH  listen on a local unix socket instead of a port
                     (service-managed sockets live in /run/ts-unplug/NAME/)
  --mode M           http | tcp (tool default: http)
  HOST[:PORT]        positional: remote tailnet host

ts-router options:
  --config PATH      routes.json to install (required on first install)
EOF
    exit 1
}

die() { echo "error: $*" >&2; exit 1; }
log() { echo ">> $*"; }

# ---------------------------------------------------------------- arg parsing

[ $# -ge 1 ] || usage
TOOL=$1; shift
case "$TOOL" in ts-plug|ts-unplug|ts-router) ;; *) usage ;; esac

NAME='' HOSTNAME_OVERRIDE='' ENV_FILE='' BINARY='' RAW_ARGS=''
VERSION='' ARCH=''
AUTHKEY="${TS_AUTHKEY:-}"
PROTO=tcp PORT='' SRC_PORT='' DST_PORT='' RUN_CMD='' PUBLIC=0
DST_SOCKET='' SRC_SOCKET='' GROUP=''
MODE='' REMOTE='' ROUTER_CONFIG=''
UNINSTALL=0 PURGE=0 DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --name)      NAME=$2; shift 2 ;;
        --hostname)  HOSTNAME_OVERRIDE=$2; shift 2 ;;
        --authkey)   AUTHKEY=$2; shift 2 ;;
        --env-file)  ENV_FILE=$2; shift 2 ;;
        --binary)    BINARY=$2; shift 2 ;;
        --version)   VERSION=$2; shift 2 ;;
        --arch)      ARCH=$2; shift 2 ;;
        --args)      RAW_ARGS=$2; shift 2 ;;
        --proto)     PROTO=$2; shift 2 ;;
        --port)      PORT=$2; shift 2 ;;
        --src-port)  SRC_PORT=$2; shift 2 ;;
        --dst-port)  DST_PORT=$2; shift 2 ;;
        --dst-socket) DST_SOCKET=$2; shift 2 ;;
        --src-socket) SRC_SOCKET=$2; shift 2 ;;
        --group)     GROUP=$2; shift 2 ;;
        --run)       RUN_CMD=$2; shift 2 ;;
        --public)    PUBLIC=1; shift ;;
        --mode)      MODE=$2; shift 2 ;;
        --config)    ROUTER_CONFIG=$2; shift 2 ;;
        --uninstall) UNINSTALL=1; shift ;;
        --purge)     PURGE=1; shift ;;
        --dry-run)   DRY_RUN=1; shift ;;
        -h|--help)   usage ;;
        --*)         die "unknown flag: $1" ;;
        *)           REMOTE=$1; shift ;;   # ts-unplug positional host
    esac
done

[ -n "$NAME" ] || die "--name is required"
case "$NAME" in *[!a-zA-Z0-9._-]*) die "--name must be alphanumeric/dot/dash/underscore" ;; esac

UNIT="$TOOL@$NAME.service"
UNIT_FILE="/etc/systemd/system/$TOOL@.service"
ENV_DIR="/etc/$TOOL"
ENV_PATH="$ENV_DIR/$NAME.env"
STATE_DIR="/var/lib/$TOOL/$NAME"
HN="${HOSTNAME_OVERRIDE:-$NAME}"

if [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    die "must run as root (or use --dry-run)"
fi

run() {
    if [ "$DRY_RUN" -eq 1 ]; then echo "DRY-RUN: $*"; else "$@"; fi
}

# ------------------------------------------------------------------ uninstall

if [ "$UNINSTALL" -eq 1 ]; then
    log "stopping and disabling $UNIT"
    run systemctl disable --now "$UNIT" || true
    run rm -f "$ENV_PATH"
    run rm -rf "/etc/systemd/system/$TOOL@$NAME.service.d"
    if [ "$PURGE" -eq 1 ]; then
        log "purging state (node keys) in $STATE_DIR"
        run rm -rf "$STATE_DIR"
        [ "$TOOL" = ts-router ] && run rm -rf "/etc/ts-router/$NAME"
    else
        log "state kept in $STATE_DIR (re-installing reuses the node identity; --purge to delete)"
    fi
    # Drop the shared template only when this was the last instance.
    if [ -d "$ENV_DIR" ] && [ -z "$(ls -A "$ENV_DIR" 2>/dev/null)" ]; then
        run rm -f "$UNIT_FILE"
        run rmdir "$ENV_DIR"
        run systemctl daemon-reload
    fi
    log "done"
    exit 0
fi

# ----------------------------------------------------------- build tool ARGS

build_ts_plug_args() {
    if [ -n "$RAW_ARGS" ]; then
        [ -n "$PORT$SRC_PORT$DST_PORT$DST_SOCKET$RUN_CMD" ] && die "--args is mutually exclusive with --port/--src-port/--dst-port/--dst-socket/--run"
        ARGS=$RAW_ARGS
        return
    fi
    case "$PROTO" in tcp|http|https|dns) ;; *) die "--proto must be tcp, http, https or dns" ;; esac
    if [ -n "$DST_SOCKET" ]; then
        [ -n "$DST_PORT" ] && die "--dst-socket and --dst-port are mutually exclusive"
        [ -n "$PORT" ]     && die "--dst-socket and --port are mutually exclusive (use --src-port)"
        [ "$PROTO" = dns ] && die "--dst-socket does not work with --proto dns"
        case "$DST_SOCKET" in /*) ;; *) die "--dst-socket must be an absolute path" ;; esac
    fi
    if [ -n "$PORT" ]; then
        [ -n "$SRC_PORT$DST_PORT" ] && die "--port is mutually exclusive with --src-port/--dst-port"
        SRC_PORT=$PORT
        DST_PORT=$PORT
    fi
    # Fill in the blanks from protocol conventions.
    case "$PROTO" in
        tcp)   SRC_PORT=${SRC_PORT:-${DST_PORT:-22}} ;;
        http)  SRC_PORT=${SRC_PORT:-80}  ;;
        https) SRC_PORT=${SRC_PORT:-443} ;;
        dns)   SRC_PORT=${SRC_PORT:-53}  ;;
    esac
    if [ "$PUBLIC" -eq 1 ] && [ "$PROTO" != https ]; then
        die "--public (Funnel) requires --proto https"
    fi
    if [ -n "$DST_SOCKET" ]; then
        ARGS="-hostname $HN -$PROTO-port $SRC_PORT:unix:$DST_SOCKET"
    else
        DST_PORT=${DST_PORT:-$SRC_PORT}
        ARGS="-hostname $HN -$PROTO-port $SRC_PORT:$DST_PORT"
    fi
    [ "$PUBLIC" -eq 1 ] && ARGS="$ARGS -public"
    # ts-plug requires an upstream command; sleep = pure forwarding mode.
    ARGS="$ARGS -- ${RUN_CMD:-/bin/sleep infinity}"
}

build_ts_unplug_args() {
    if [ -n "$RAW_ARGS" ]; then ARGS=$RAW_ARGS; return; fi
    [ -n "$REMOTE" ] || die "ts-unplug needs the remote tailnet host as a positional argument"
    if [ -n "$SRC_SOCKET" ]; then
        [ -n "$PORT" ] && die "--src-socket and --port are mutually exclusive"
        case "$SRC_SOCKET" in /*) ;; *) die "--src-socket must be an absolute path" ;; esac
        ARGS="-hostname $HN -socket $SRC_SOCKET"
    else
        [ -n "$PORT" ] || die "ts-unplug needs --port (local listen port) or --src-socket"
        ARGS="-hostname $HN -port $PORT"
    fi
    [ -n "$MODE" ] && ARGS="$ARGS -mode $MODE"
    ARGS="$ARGS $REMOTE"
}

build_ts_router_args() {
    if [ -n "$RAW_ARGS" ]; then ARGS=$RAW_ARGS; return; fi
    [ -n "$ROUTER_CONFIG" ] || [ -f "/etc/ts-router/$NAME/routes.json" ] \
        || die "ts-router needs --config routes.json on first install"
    ARGS="-hostname $HN"
}

case "$TOOL" in
    ts-plug)   build_ts_plug_args ;;
    ts-unplug) build_ts_unplug_args ;;
    ts-router) build_ts_router_args ;;
esac

# ------------------------------------------------------------- binary lookup

detect_arch() {
    if [ -n "$ARCH" ]; then echo "$ARCH"; return; fi
    case "$(uname -m)" in
        x86_64)  echo amd64 ;;
        aarch64) echo arm64 ;;
        armv7l)  echo armv7 ;;
        *) die "unsupported architecture '$(uname -m)': pass --arch amd64|arm64|armv7 or --binary" ;;
    esac
}

fetch() {  # fetch URL DEST
    if command -v curl >/dev/null; then
        curl -fsSL -o "$2" "$1"
    elif command -v wget >/dev/null; then
        wget -qO "$2" "$1"
    else
        die "need curl or wget to download release binaries"
    fi
}

download_binary() {
    local arch asset base
    [ "$(uname -s)" = Linux ] || die "release download is Linux-only (this is a systemd installer)"
    arch=$(detect_arch)
    asset="$TOOL-linux-$arch"
    if [ -z "$VERSION" ] || [ "$VERSION" = latest ]; then
        base="https://github.com/$GH_REPO/releases/latest/download"
    else
        base="https://github.com/$GH_REPO/releases/download/$VERSION"
    fi
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "DRY-RUN: download + sha256-verify $base/$asset" >&2
        echo "/tmp/dry-run/$asset"; return
    fi
    local tmp
    tmp=$(mktemp -d /tmp/ts-plug-install.XXXXXX)
    log "downloading $base/$asset" >&2
    fetch "$base/$asset" "$tmp/$asset" || die "download failed: $base/$asset (does the release exist?)"
    fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS" || die "download failed: $base/SHA256SUMS"
    (cd "$tmp" && grep "  $asset\$" SHA256SUMS | sha256sum -c --quiet -) >&2 \
        || die "checksum verification failed for $asset"
    chmod +x "$tmp/$asset"
    echo "$tmp/$asset"
}

resolve_binary() {
    if [ -n "$BINARY" ]; then
        [ -n "$VERSION" ] && die "--binary and --version are mutually exclusive"
        [ -x "$BINARY" ] || die "--binary $BINARY is not executable"
        echo "$BINARY"; return
    fi
    if [ -n "$VERSION" ]; then    # explicit pin always downloads
        download_binary; return
    fi
    if [ -n "$REPO_ROOT" ] && [ -x "$REPO_ROOT/build/$TOOL" ]; then
        echo "$REPO_ROOT/build/$TOOL"; return
    fi
    if [ -n "$REPO_ROOT" ] && [ -f "$REPO_ROOT/Makefile" ] && [ -d "$REPO_ROOT/cmd" ] && command -v go >/dev/null; then
        log "building $TOOL from source" >&2
        make -C "$REPO_ROOT" "$TOOL" >&2
        echo "$REPO_ROOT/build/$TOOL"; return
    fi
    if [ -x "$BINDIR/$TOOL" ]; then
        echo ""; return   # already installed, nothing to copy
    fi
    download_binary       # last resort: latest release
}

SRC_BINARY=$(resolve_binary)

# ------------------------------------------------------------------ auth key

# Precedence: --authkey / $TS_AUTHKEY (already in AUTHKEY), --env-file,
# existing installed env file, interactive prompt.
if [ -z "$AUTHKEY" ] && [ -n "$ENV_FILE" ]; then
    [ -f "$ENV_FILE" ] || die "--env-file $ENV_FILE not found"
    AUTHKEY=$(sed -n 's/^TS_AUTHKEY=//p' "$ENV_FILE" | head -n1)
fi
if [ -z "$AUTHKEY" ] && [ -f "$ENV_PATH" ]; then
    AUTHKEY=$(sed -n 's/^TS_AUTHKEY=//p' "$ENV_PATH" | head -n1)
    [ -n "$AUTHKEY" ] && log "reusing TS_AUTHKEY from $ENV_PATH"
fi
if [ -z "$AUTHKEY" ] && [ -d "$STATE_DIR" ]; then
    log "no auth key, but $STATE_DIR exists — assuming the node is already joined"
fi
if [ -z "$AUTHKEY" ] && [ ! -d "$STATE_DIR" ]; then
    if [ -t 0 ]; then
        printf 'Tailscale auth key (tskey-auth-..., input hidden): ' >&2
        read -rs AUTHKEY; echo >&2
        [ -n "$AUTHKEY" ] || die "no auth key given and no existing state — the node cannot join"
    elif [ "$DRY_RUN" -eq 1 ]; then
        AUTHKEY="tskey-auth-DRYRUN"
    else
        die "no auth key: pass --authkey, --env-file, or set TS_AUTHKEY"
    fi
fi

# -------------------------------------------------------------- unit content

unit_content() {
    case "$TOOL" in
    ts-plug) cat <<'EOF' ;;
[Unit]
Description=ts-plug (%i): expose a local service to the tailnet
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ts-plug/%i.env
DynamicUser=yes
StateDirectory=ts-plug/%i
ExecStart=/usr/local/bin/ts-plug -dir ${STATE_DIRECTORY} $ARGS
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    ts-unplug) cat <<'EOF' ;;
[Unit]
Description=ts-unplug (%i): bring a tailnet service to localhost
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ts-unplug/%i.env
DynamicUser=yes
StateDirectory=ts-unplug/%i
# Writable home for -socket listeners: /run/ts-unplug/<instance>/
RuntimeDirectory=ts-unplug/%i
ExecStart=/usr/local/bin/ts-unplug -dir ${STATE_DIRECTORY} $ARGS
Restart=on-failure
RestartSec=5
# Uncomment to let ts-unplug bind local ports below 1024:
#AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
    ts-router) cat <<'EOF' ;;
[Unit]
Description=ts-router (%i): route tailnet hosts to localhost under real URLs
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/ts-router/%i.env
DynamicUser=yes
StateDirectory=ts-router/%i
ExecStart=/usr/local/bin/ts-router -dir ${STATE_DIRECTORY} -config /etc/ts-router/%i/routes.json $ARGS
Restart=on-failure
RestartSec=5
# routes typically bind 127.0.0.1:80/:443
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
    esac
}

env_content() {
    echo "# Managed by install-systemd.sh — instance '$NAME' of $TOOL"
    [ -n "$AUTHKEY" ] && echo "TS_AUTHKEY=$AUTHKEY"
    echo "ARGS=$ARGS"
}

dropin_content() {
    echo "# Managed by install-systemd.sh (--group $GROUP)"
    echo "[Service]"
    echo "SupplementaryGroups=$GROUP"
}

# --------------------------------------------------------------------- apply

if [ "$DRY_RUN" -eq 1 ]; then
    echo "---- $UNIT_FILE"; unit_content
    echo "---- $ENV_PATH (mode 0600)"; env_content
fi

if [ -n "$SRC_BINARY" ]; then
    log "installing $SRC_BINARY -> $BINDIR/$TOOL"
    run install -m 0755 "$SRC_BINARY" "$BINDIR/$TOOL"
    # Downloaded binaries live in a throwaway dir; clean it up after install.
    case "$SRC_BINARY" in
        /tmp/ts-plug-install.*) [ "$DRY_RUN" -eq 0 ] && rm -rf "$(dirname "$SRC_BINARY")" ;;
    esac
fi

log "writing $UNIT_FILE"
if [ "$DRY_RUN" -eq 0 ]; then unit_content > "$UNIT_FILE"; fi

log "writing $ENV_PATH"
if [ "$DRY_RUN" -eq 0 ]; then
    install -d -m 0755 "$ENV_DIR"
    umask 077
    env_content > "$ENV_PATH"
    umask 022
fi

DROPIN_DIR="/etc/systemd/system/$TOOL@$NAME.service.d"
if [ -n "$GROUP" ]; then
    if [ "$DRY_RUN" -eq 0 ]; then
        getent group "$GROUP" >/dev/null || die "group '$GROUP' does not exist"
    fi
    log "writing $DROPIN_DIR/group.conf (SupplementaryGroups=$GROUP)"
    if [ "$DRY_RUN" -eq 1 ]; then
        echo "---- $DROPIN_DIR/group.conf"; dropin_content
    else
        install -d -m 0755 "$DROPIN_DIR"
        dropin_content > "$DROPIN_DIR/group.conf"
    fi
elif [ -f "$DROPIN_DIR/group.conf" ]; then
    # --group was dropped on a re-install: remove the stale drop-in.
    log "removing stale $DROPIN_DIR/group.conf"
    run rm -f "$DROPIN_DIR/group.conf"
    run rmdir --ignore-fail-on-non-empty "$DROPIN_DIR"
fi

if [ "$TOOL" = ts-router ] && [ -n "$ROUTER_CONFIG" ]; then
    [ -f "$ROUTER_CONFIG" ] || die "--config $ROUTER_CONFIG not found"
    log "installing routes config -> /etc/ts-router/$NAME/routes.json"
    run install -d -m 0755 "/etc/ts-router/$NAME"
    run install -m 0644 "$ROUTER_CONFIG" "/etc/ts-router/$NAME/routes.json"
fi

log "enabling and starting $UNIT"
run systemctl daemon-reload
run systemctl enable --now "$UNIT"

if [ "$DRY_RUN" -eq 0 ]; then
    sleep 2
    systemctl --no-pager --lines=0 status "$UNIT" || true
fi

# -------------------------------------------------------------------- report

echo
log "installed $UNIT"
log "state (node keys, certs): $STATE_DIR"
log "config:                   $ENV_PATH"
case "$TOOL" in
    ts-plug)
        if [ -z "$RAW_ARGS" ]; then
            DST_LABEL="127.0.0.1:$DST_PORT"
            [ -n "$DST_SOCKET" ] && DST_LABEL="unix:$DST_SOCKET"
            case "$PROTO" in
                tcp)
                    if [ "$SRC_PORT" = 22 ]; then
                        log "reach it: ssh <user>@$HN.<your-tailnet>.ts.net"
                    else
                        log "reach it: $HN.<your-tailnet>.ts.net:$SRC_PORT (raw tcp -> $DST_LABEL)"
                    fi ;;
                https) log "reach it: https://$HN.<your-tailnet>.ts.net/ -> $DST_LABEL" ;;
                http)  log "reach it: http://$HN.<your-tailnet>.ts.net/ -> $DST_LABEL" ;;
                dns)   log "reach it: dig @$HN.<your-tailnet>.ts.net (udp $SRC_PORT -> 127.0.0.1:$DST_PORT)" ;;
            esac
        fi ;;
    ts-unplug)
        if [ -n "$SRC_SOCKET" ]; then
            log "reach it: unix:$SRC_SOCKET -> $REMOTE"
        else
            log "reach it: 127.0.0.1:$PORT -> $REMOTE"
        fi ;;
    ts-router)
        log "if using DNS routes: sudo $BINDIR/ts-router -config /etc/ts-router/$NAME/routes.json install-resolved" ;;
esac
log "logs: journalctl -fu $UNIT"
log "once joined, TS_AUTHKEY in $ENV_PATH may be removed; identity persists in $STATE_DIR"
