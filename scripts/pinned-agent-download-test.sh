#!/bin/sh
# 用本地产物替身验收：未给出清单、清单不合法、摘要不匹配、下载失败都不能变成通过；不访问网络或执行发布二进制。
# AGENT_PIN 是清单的唯一入口。脚本旁边即使放着一份合法清单，未设置时也必须在调用 curl 之前失败。
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir "$work/scripts" "$work/bin" "$work/package"
cp "$root/scripts/pinned-agent-download.sh" "$work/scripts/"
printf '#!/bin/sh\nprintf "published fixture\\n"\n' > "$work/package/heron-agent"
chmod +x "$work/package/heron-agent"
COPYFILE_DISABLE=1 tar --no-xattrs -czf "$work/fixture.tar.gz" -C "$work/package" heron-agent
digest=$(sha256sum "$work/fixture.tar.gz")
digest=${digest%% *}
# 样本是一份合法清单：仓库、正式版 tag，以及 amd64 与 arm64 的摘要。
jq -n --arg digest "$digest" '{repository: "xjetry/heron", tag: "v9.9.0", assets: [{arch: "amd64", sha256: $digest}, {arch: "arm64", sha256: $digest}]}' > "$work/pin.json"
cat > "$work/bin/curl" <<'SH'
#!/bin/sh
set -eu
echo called >> "$FIXTURE_ROOT/curl-calls"
if [ "${FAIL_DOWNLOAD:-0}" = 1 ]; then exit 22; fi
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://*) url=$1 ;;
    -o) shift; out=$1 ;;
  esac
  shift
done
case "$url" in
  https://github.com/xjetry/heron/releases/download/v9.9.0/heron-agent_linux_amd64.tar.gz|https://github.com/xjetry/heron/releases/download/v9.9.0/heron-agent_linux_arm64.tar.gz|https://github.com/xjetry/heron/releases/download/v9.8.0/heron-agent_linux_amd64.tar.gz|https://github.com/xjetry/heron/releases/download/v9.8.0/heron-agent_linux_arm64.tar.gz) ;;
  *) echo "unexpected release URL: $url" >&2; exit 1 ;;
esac
cp "$FIXTURE_ROOT/fixture.tar.gz" "$out"
SH
chmod +x "$work/bin/curl"
export FIXTURE_ROOT="$work"
export PATH="$work/bin:$PATH"

rejects() {
  label=$1; want=$2
  rc=0
  "$work/scripts/pinned-agent-download.sh" "$work/output" > "$work/result" 2>&1 || rc=$?
  [ "$rc" != 0 ] || { echo "FAIL: $label unexpectedly succeeded" >&2; exit 1; }
  grep -F "$want" "$work/result" > /dev/null || { echo "FAIL: $label failed for the wrong reason" >&2; cat "$work/result" >&2; exit 1; }
  [ ! -e "$work/output" ] || { echo "FAIL: $label retained unverified artifacts" >&2; exit 1; }
  echo "rejected $label: exit $rc ($want)"
}

# 未设置 AGENT_PIN 时必须在调用 curl 之前失败，失败原因是 AGENT_PIN is required。
# 脚本目录里放一份合法清单，证明失败不取决于旁边有没有可读的文件。
cp "$work/pin.json" "$work/scripts/pin.json"
unset AGENT_PIN || true
rejects "AGENT_PIN unset" "AGENT_PIN is required"
[ ! -e "$work/curl-calls" ] || { echo "FAIL: AGENT_PIN unset attempted a download" >&2; exit 1; }
rm "$work/scripts/pin.json"

AGENT_PIN="$work/pin.json" "$work/scripts/pinned-agent-download.sh" "$work/output" > "$work/result" 2>&1
grep -F "xjetry/heron v9.9.0" "$work/result" > /dev/null || { echo "FAIL: download did not follow the AGENT_PIN tag" >&2; cat "$work/result" >&2; exit 1; }
for arch in amd64 arm64; do
  [ "$("$work/output/heron-agent-linux-$arch")" = "published fixture" ] || { echo "FAIL: missing verified $arch agent" >&2; exit 1; }
done
[ "$(awk 'END { print NR }' "$work/curl-calls")" = 2 ] || { echo "FAIL: did not download both release assets" >&2; exit 1; }
rm -rf "$work/output"
rm "$work/curl-calls"

AGENT_PIN="$work/missing.json" rejects "missing pin" "invalid or absent published-agent pin"
[ ! -e "$work/curl-calls" ] || { echo "FAIL: missing pin attempted a download" >&2; exit 1; }
jq '.assets = [.assets[0]]' "$work/pin.json" > "$work/bad.json"
AGENT_PIN="$work/bad.json" rejects "missing architecture" "invalid or absent published-agent pin"
[ ! -e "$work/curl-calls" ] || { echo "FAIL: incomplete pin attempted a download" >&2; exit 1; }
jq '.tag = "latest"' "$work/pin.json" > "$work/bad.json"
AGENT_PIN="$work/bad.json" rejects "mutable release tag" "invalid or absent published-agent pin"
[ ! -e "$work/curl-calls" ] || { echo "FAIL: invalid release identity attempted a download" >&2; exit 1; }

# 故意换掉摘要，先确认写入值与真实摘要不同，再跑同一下载入口。
jq '.assets[0].sha256 = "0000000000000000000000000000000000000000000000000000000000000000"' "$work/pin.json" > "$work/bad.json"
[ "$(jq -r '.assets[0].sha256' "$work/bad.json")" != "$digest" ] || { echo "FAIL: checksum fault was not injected" >&2; exit 1; }
AGENT_PIN="$work/bad.json" rejects "checksum mismatch" "SHA256 mismatch"
[ "$(awk 'END { print NR }' "$work/curl-calls")" = 1 ] || { echo "FAIL: checksum failure did not stop after the first asset" >&2; exit 1; }
rm "$work/curl-calls"

export FAIL_DOWNLOAD=1
rc=0
AGENT_PIN="$work/pin.json" "$work/scripts/pinned-agent-download.sh" "$work/output" > "$work/result" 2>&1 || rc=$?
if ! { [ "$rc" = 22 ] && [ ! -e "$work/output" ]; }; then echo "FAIL: failed release download was skipped or left artifacts (exit $rc)" >&2; cat "$work/result" >&2; exit 1; fi
unset FAIL_DOWNLOAD
echo "rejected unavailable release: exit $rc"

# 恢复摘要与下载后仍须通过，避免负例破坏替身而产生虚假的验红。
AGENT_PIN="$work/pin.json" "$work/scripts/pinned-agent-download.sh" "$work/output" > "$work/result" 2>&1
for arch in amd64 arm64; do
  [ "$("$work/output/heron-agent-linux-$arch")" = "published fixture" ] || { echo "FAIL: restored $arch baseline failed" >&2; exit 1; }
done

# 另一份清单的 tag 必须被照做：调用方传哪份清单，下载就核哪份，不能沿用上一次的 tag。
rm -rf "$work/output"
jq -n --arg digest "$digest" '{repository: "xjetry/heron", tag: "v9.8.0", assets: [{arch: "amd64", sha256: $digest}, {arch: "arm64", sha256: $digest}]}' > "$work/other.json"
rm -f "$work/curl-calls"
rc=0
AGENT_PIN="$work/other.json" "$work/scripts/pinned-agent-download.sh" "$work/output" > "$work/result" 2>&1 || rc=$?
if [ "$rc" != 0 ]; then echo "FAIL: the second AGENT_PIN was not used" >&2; cat "$work/result" >&2; exit 1; fi
grep -F "xjetry/heron v9.8.0" "$work/result" > /dev/null || { echo "FAIL: download did not follow the second AGENT_PIN tag" >&2; cat "$work/result" >&2; exit 1; }
[ "$(awk 'END { print NR }' "$work/curl-calls")" = 2 ] || { echo "FAIL: the second AGENT_PIN did not download both release assets" >&2; exit 1; }
for arch in amd64 arm64; do
  [ "$("$work/output/heron-agent-linux-$arch")" = "published fixture" ] || { echo "FAIL: second AGENT_PIN missing $arch agent" >&2; exit 1; }
done
echo "pinned agent download checks OK"
