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

set -euo pipefail

BINDIR=/usr/local/bin
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." 2>/dev/null && pwd || true)"

usage() {
    cat <<'EOF'
usage: install-systemd.sh <ts-plug|ts-unplug|ts-router> --name NAME [options]

common options:
  --name NAME        instance name; also the default tailnet hostname (required)
  --hostname HN      tailnet hostname if different from --name
  --authkey KEY      tailscale auth key (tskey-auth-...)
  --env-file PATH    read TS_AUTHKEY from an existing env file
  --binary PATH      install this binary instead of building/looking one up
  --args 'RAW'       raw tool arguments; escape hatch, replaces generated args
  --uninstall        stop, disable and remove the instance
  --purge            with --uninstall: also delete state (node keys!) and config
  --dry-run          print what would be done without touching anything

ts-plug options (expose 127.0.0.1 to the tailnet):
  --proto P          tcp | http | https | dns        (default: tcp)
  --port N           listen and target port           (tcp default: 22)
  --src-port N       tailnet-side listen port
  --dst-port N       local destination port
  --run 'CMD'        upstream command ts-plug should supervise
                     (default: /bin/sleep infinity — i.e. plain forwarding)
  --public           enable Tailscale Funnel (https only)

ts-unplug options (bring a tailnet service to 127.0.0.1):
  --port N           local listen port (required)
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
AUTHKEY="${TS_AUTHKEY:-}"
PROTO=tcp PORT='' SRC_PORT='' DST_PORT='' RUN_CMD='' PUBLIC=0
MODE='' REMOTE='' ROUTER_CONFIG=''
UNINSTALL=0 PURGE=0 DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --name)      NAME=$2; shift 2 ;;
        --hostname)  HOSTNAME_OVERRIDE=$2; shift 2 ;;
        --authkey)   AUTHKEY=$2; shift 2 ;;
        --env-file)  ENV_FILE=$2; shift 2 ;;
        --binary)    BINARY=$2; shift 2 ;;
        --args)      RAW_ARGS=$2; shift 2 ;;
        --proto)     PROTO=$2; shift 2 ;;
        --port)      PORT=$2; shift 2 ;;
        --src-port)  SRC_PORT=$2; shift 2 ;;
        --dst-port)  DST_PORT=$2; shift 2 ;;
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
        [ -n "$PORT$SRC_PORT$DST_PORT$RUN_CMD" ] && die "--args is mutually exclusive with --port/--src-port/--dst-port/--run"
        ARGS=$RAW_ARGS
        return
    fi
    case "$PROTO" in tcp|http|https|dns) ;; *) die "--proto must be tcp, http, https or dns" ;; esac
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
    DST_PORT=${DST_PORT:-$SRC_PORT}
    if [ "$PUBLIC" -eq 1 ] && [ "$PROTO" != https ]; then
        die "--public (Funnel) requires --proto https"
    fi
    ARGS="-hostname $HN -$PROTO-port $SRC_PORT:$DST_PORT"
    [ "$PUBLIC" -eq 1 ] && ARGS="$ARGS -public"
    # ts-plug requires an upstream command; sleep = pure forwarding mode.
    ARGS="$ARGS -- ${RUN_CMD:-/bin/sleep infinity}"
}

build_ts_unplug_args() {
    if [ -n "$RAW_ARGS" ]; then ARGS=$RAW_ARGS; return; fi
    [ -n "$PORT" ]   || die "ts-unplug needs --port (local listen port)"
    [ -n "$REMOTE" ] || die "ts-unplug needs the remote tailnet host as a positional argument"
    ARGS="-hostname $HN -port $PORT"
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

resolve_binary() {
    if [ -n "$BINARY" ]; then
        [ -x "$BINARY" ] || die "--binary $BINARY is not executable"
        echo "$BINARY"; return
    fi
    if [ -n "$REPO_ROOT" ] && [ -x "$REPO_ROOT/build/$TOOL" ]; then
        echo "$REPO_ROOT/build/$TOOL"; return
    fi
    if [ -n "$REPO_ROOT" ] && [ -f "$REPO_ROOT/Makefile" ] && command -v go >/dev/null; then
        log "building $TOOL from source" >&2
        make -C "$REPO_ROOT" "$TOOL" >&2
        echo "$REPO_ROOT/build/$TOOL"; return
    fi
    if [ -x "$BINDIR/$TOOL" ]; then
        echo "";  return   # already installed, nothing to copy
    fi
    die "no $TOOL binary found: pass --binary, or run from the repo with go installed"
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

# --------------------------------------------------------------------- apply

if [ "$DRY_RUN" -eq 1 ]; then
    echo "---- $UNIT_FILE"; unit_content
    echo "---- $ENV_PATH (mode 0600)"; env_content
fi

if [ -n "$SRC_BINARY" ]; then
    log "installing $SRC_BINARY -> $BINDIR/$TOOL"
    run install -m 0755 "$SRC_BINARY" "$BINDIR/$TOOL"
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
            case "$PROTO" in
                tcp)
                    if [ "$SRC_PORT" = 22 ]; then
                        log "reach it: ssh <user>@$HN.<your-tailnet>.ts.net"
                    else
                        log "reach it: $HN.<your-tailnet>.ts.net:$SRC_PORT (raw tcp -> 127.0.0.1:$DST_PORT)"
                    fi ;;
                https) log "reach it: https://$HN.<your-tailnet>.ts.net/ -> 127.0.0.1:$DST_PORT" ;;
                http)  log "reach it: http://$HN.<your-tailnet>.ts.net/ -> 127.0.0.1:$DST_PORT" ;;
                dns)   log "reach it: dig @$HN.<your-tailnet>.ts.net (udp $SRC_PORT -> 127.0.0.1:$DST_PORT)" ;;
            esac
        fi ;;
    ts-unplug)
        log "reach it: 127.0.0.1:$PORT -> $REMOTE" ;;
    ts-router)
        log "if using DNS routes: sudo $BINDIR/ts-router -config /etc/ts-router/$NAME/routes.json install-resolved" ;;
esac
log "logs: journalctl -fu $UNIT"
log "once joined, TS_AUTHKEY in $ENV_PATH may be removed; identity persists in $STATE_DIR"
