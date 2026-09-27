#!/bin/sh
# Run as root on Linux with systemd. Defaults match the user's server paths.
set -eu
umask 077

fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
require() { command -v "$1" >/dev/null 2>&1 || fail "Required command not found: $1"; }
safe_path() {
    case "$1" in
        /*) ;;
        *) fail "An absolute path is required: $1" ;;
    esac
    case "$1" in
        *[!a-zA-Z0-9_./-]*) fail "Path contains unsupported characters: $1" ;;
    esac
}

[ "$(id -u)" -eq 0 ] || fail 'Run this deployment script as root (sudo sh deploy.sh).'
[ "$(uname -s)" = Linux ] || fail 'This deployment script requires Linux and systemd.'
for command in git go systemctl install mktemp getent useradd cp mv readlink; do require "$command"; done
systemctl show-environment >/dev/null || fail 'systemd is not running.'

INSTALL_DIR=${INSTALL_DIR:-/usr/local/title_master}
REPO_URL=${TITLE_MASTER_REPO_URL:-https://github.com/userreksai/title-master.git}
REF=${TITLE_MASTER_REF:-}
SERVICE_USER=title-master
SERVICE=title-master.service
UNIT_DIR=${SYSTEMD_UNIT_DIR:-/etc/systemd/system}
safe_path "$UNIT_DIR"
UNIT_PATH=$UNIT_DIR/$SERVICE
safe_path "$INSTALL_DIR"
install -d -m 755 "$INSTALL_DIR" "$UNIT_DIR"
INSTALL_DIR=$(cd "$INSTALL_DIR" && pwd -P)
case "$INSTALL_DIR" in /|/usr|/usr/local|/user|/user/local|/etc|/var|/var/lib) fail 'Use a dedicated title_master directory.' ;; esac
CONFIG=$INSTALL_DIR/master.json
BINARY=$INSTALL_DIR/title-master
work_dir=$(mktemp -d "$INSTALL_DIR/.deploy.XXXXXX")
cleanup() { [ ! -d "$work_dir" ] || rm -rf -- "$work_dir"; }
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'Downloading latest title-master source...\n'
if [ -n "$REF" ]; then
    git clone --quiet --depth 1 --no-tags --branch "$REF" -- "$REPO_URL" "$work_dir/source"
else
    git clone --quiet --depth 1 --no-tags -- "$REPO_URL" "$work_dir/source"
fi
revision=$(git -C "$work_dir/source" rev-parse HEAD)
(
    cd "$work_dir/source"
    printf 'Building commit %s with %s\n' "$revision" "$(go version)"
    CGO_ENABLED=0 go build -trimpath -o "$work_dir/title-master" ./cmd/title-master
)

if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --user-group --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
fi
SERVICE_GROUP=$(id -gn "$SERVICE_USER")
if [ ! -e "$CONFIG" ]; then
    install -o root -g "$SERVICE_GROUP" -m 640 "$work_dir/source/configs/master.example.json" "$CONFIG"
fi

configured=1
if ! "$work_dir/title-master" -config "$CONFIG" -check-config; then
    configured=0
    # Never disturb a previous installation if its config fails validation.
    if [ -e "$BINARY" ] || [ -e "$UNIT_PATH" ]; then
        fail "Configuration invalid. Existing binary/service left unchanged. Edit $CONFIG, then run this script again."
    fi
fi
if [ "$configured" -eq 1 ]; then
    state_path=$("$work_dir/title-master" -config "$CONFIG" -print-state-path)
else
    state_path=$INSTALL_DIR/data/state.json
fi
safe_path "$state_path"
state_dir=$(dirname "$state_path")
state_dir=$(readlink -m "$state_dir")
safe_path "$state_dir"
case "$state_dir" in /|/usr|/usr/local|/user|/user/local|/etc|/var|/var/lib|"$INSTALL_DIR")
    fail 'state_file must be inside a dedicated data directory (default: data/state.json).'
    ;;
esac
install -d -o "$SERVICE_USER" -g "$SERVICE_GROUP" -m 700 "$state_dir"
# Preserve the baseline and outbox, including when adopting an older install.
for file in "$state_path" "$state_path.lock"; do
    if [ -e "$file" ]; then chown "$SERVICE_USER:$SERVICE_GROUP" "$file"; chmod 600 "$file"; fi
done
chown root:"$SERVICE_GROUP" "$CONFIG"
chmod 640 "$CONFIG"

cat > "$work_dir/$SERVICE" <<EOF
[Unit]
Description=SEO Title Master
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_GROUP
WorkingDirectory=$INSTALL_DIR
ExecStartPre=$BINARY -config $CONFIG -check-config
ExecStart=$BINARY -config $CONFIG
Restart=on-failure
RestartSec=5
TimeoutStopSec=30
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=$state_dir
UMask=0077

[Install]
WantedBy=multi-user.target
EOF

was_active=0
was_enabled=0
systemctl is-active --quiet "$SERVICE" && was_active=1
systemctl is-enabled --quiet "$SERVICE" && was_enabled=1
if [ -e "$BINARY" ]; then cp -p "$BINARY" "$work_dir/previous-binary"; cp -p "$BINARY" "$BINARY.previous"; fi
if [ -e "$UNIT_PATH" ]; then cp -p "$UNIT_PATH" "$work_dir/previous-unit"; fi

rollback() {
    printf 'Deployment failed; restoring previous binary and unit.\n' >&2
    systemctl stop "$SERVICE" >/dev/null 2>&1 || :
    if [ -e "$work_dir/previous-binary" ]; then mv -f "$work_dir/previous-binary" "$BINARY"; else rm -f -- "$BINARY"; fi
    if [ -e "$work_dir/previous-unit" ]; then cp -p "$work_dir/previous-unit" "$UNIT_PATH"; else rm -f -- "$UNIT_PATH"; fi
    if [ "$was_enabled" -eq 0 ]; then systemctl disable "$SERVICE" >/dev/null 2>&1 || :; fi
    systemctl daemon-reload || :
    if [ "$was_active" -eq 1 ]; then systemctl restart "$SERVICE" || :; fi
    printf 'Configuration and state data were kept. Check: journalctl -u %s -n 50\n' "$SERVICE" >&2
}
install -m 755 "$work_dir/title-master" "$INSTALL_DIR/.title-master.next"
mv -f "$INSTALL_DIR/.title-master.next" "$BINARY"
if ! install -m 644 "$work_dir/$SERVICE" "$UNIT_PATH"; then rollback; exit 1; fi
if ! systemctl daemon-reload; then rollback; exit 1; fi

if [ "$configured" -eq 0 ]; then
    printf '\nInstalled binary and systemd unit; service has NOT been started.\n'
    printf 'Edit %s (SEO API/token, agents/token, webhooks), then rerun this deployment script.\n' "$CONFIG"
    exit 0
fi
if ! systemctl enable "$SERVICE"; then rollback; exit 1; fi
if ! systemctl restart "$SERVICE"; then rollback; exit 1; fi
sleep 3
if ! systemctl is-active --quiet "$SERVICE"; then rollback; exit 1; fi
printf '%s\n' "$revision" > "$INSTALL_DIR/title-master.version"
printf '\nDeployed: %s\nCommit: %s\nState: %s\n' "$BINARY" "$revision" "$state_path"
printf 'Service: systemctl status title-master\nLogs: journalctl -u title-master -f\n'
