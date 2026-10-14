#!/bin/sh
# 仅在专用隔离机中运行，真实加载两种更新器单元并验证子进程降权。
set -eu
case "$(hostname)" in pia-update*) ;; *) echo 'requires disposable pia-update machine' >&2; exit 1;; esac
[ "$(id -u)" = 0 ]
repo=$1
binary=$2
test -f /var/lib/heron-update-accept
work=$(mktemp -d /var/lib/heron-credentials.XXXXXX)
trap 'rm -rf "$work"' EXIT
install -m 0755 "$binary" /usr/local/bin/heron-credential-tests
for role in hub agent; do
  if ! id "heron-$role" >/dev/null 2>&1; then useradd --system --no-create-home "heron-$role"; fi
  mkdir -p /var/lib/heron
  unit="heron-credential-$role.service"
  install -m 0644 "$repo/deploy/systemd/heron-updater-$role.service" "/etc/systemd/system/$unit"
  mkdir -p "/etc/systemd/system/$unit.d"
  # 名称排在 OrbStack 的全局加固覆盖之后，否则其 NoNewPrivileges=no 会掩盖权限缺陷。
  {
    printf '[Service]\n'
    sed -n '/^NoNewPrivileges=/p; /^ProtectSystem=/p; /^ProtectHome=/p; /^PrivateTmp=/p; /^ReadWritePaths=/p' "$repo/deploy/systemd/heron-updater-$role.service"
    printf 'Type=oneshot\nRestart=no\nExecStart=\nExecStart=/usr/local/bin/heron-credential-tests -test.run=^TestSystemCredentialDrop$ -test.count=1 -test.v\n'
    printf 'Environment=HERON_UPDATE_ACCEPT=isolated-systemd HERON_UPDATE_ROLE=%s\n' "$role"
    printf 'StandardOutput=file:%s/%s.log\nStandardError=inherit\n' "$work" "$role"
  } > "/etc/systemd/system/$unit.d/zzzz-credentials.conf"
  systemctl daemon-reload
  result=0
  since=$(date -u '+%Y-%m-%d %H:%M:%S UTC')
  systemctl start "$unit" || result=$?
  journalctl -u "$unit" --since "$since" --no-pager
  cat "$work/$role.log"
  [ "$result" = 0 ] || exit "$result"
  grep -qx 'HERON_CREDENTIAL_PARENT_VERIFIED' "$work/$role.log"
done
