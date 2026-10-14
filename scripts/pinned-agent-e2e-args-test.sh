#!/bin/sh
# make -n：PINNED_AGENT_VERSION 只写进下载清单的版本；hub 的 -X main.agentVersion 仍是导出的 AGENT_VERSION。
# 两者若被收成同一个输入，dry-run 里 hub 那一行会带上钉住的版本，或把 AGENT_VERSION 改写成它。
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
bound=$(sed -n 1p AGENT_VERSION)
pin=v9.9.9
[ -n "$bound" ] || { echo "FAIL: AGENT_VERSION is empty" >&2; exit 1; }
[ "$bound" != "$pin" ] || { echo "FAIL: AGENT_VERSION is $pin; the pin override proves nothing" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
# 外层 make 的 MAKEFLAGS 会把 -n 以外的开关和命令行变量传进来，盖住这里要观察的那一组。
unset MAKEFLAGS MFLAGS MAKELEVEL
make -n pinned-agent-e2e PINNED_AGENT_VERSION="$pin" > "$work/n" 2>&1 || {
  echo "FAIL: make -n pinned-agent-e2e exited $?" >&2
  cat "$work/n" >&2
  exit 1
}

grep -F './cmd/hub' "$work/n" | grep -F 'main.agentVersion=' > "$work/hub-lines" || true
[ -s "$work/hub-lines" ] || { echo "FAIL: dry-run has no hub binding injection" >&2; cat "$work/n" >&2; exit 1; }
while IFS= read -r line; do
  # shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$AGENT_VERSION 按字面出现
  case $line in
    *'main.agentVersion=$AGENT_VERSION'*) ;;
    *) echo "FAIL: hub binding injection is not \$AGENT_VERSION: $line" >&2; exit 1 ;;
  esac
  case $line in
    *"$pin"*|*PINNED_AGENT_VERSION*) echo "FAIL: hub binding injection follows the pinned download version: $line" >&2; exit 1 ;;
  esac
done < "$work/hub-lines"

# shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$PINNED_AGENT_VERSION 按字面出现
if grep -F "AGENT_VERSION=$pin" "$work/n" > /dev/null \
  || grep -F "AGENT_VERSION=\"$pin\"" "$work/n" > /dev/null \
  || grep -F 'AGENT_VERSION=$PINNED_AGENT_VERSION' "$work/n" > /dev/null \
  || grep -F 'AGENT_VERSION="$PINNED_AGENT_VERSION"' "$work/n" > /dev/null; then
  echo "FAIL: dry-run assigns AGENT_VERSION from the pinned download version" >&2
  cat "$work/n" >&2
  exit 1
fi

grep -F 'boundagent pin' "$work/n" > "$work/pin-lines" || true
[ -s "$work/pin-lines" ] || { echo "FAIL: dry-run does not run boundagent pin" >&2; cat "$work/n" >&2; exit 1; }
while IFS= read -r line; do
  case $line in
    *"-version \"$pin\""*|*"-version $pin "*|*"-version $pin") ;;
    *) echo "FAIL: pin -version is not $pin: $line" >&2; exit 1 ;;
  esac
  # shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$AGENT_VERSION 按字面出现
  case $line in
    *"-version \"$bound\""*|*"-version $bound "*|*"-version $bound"|*'-version $AGENT_VERSION'*|*'-version "$AGENT_VERSION"'*)
      echo "FAIL: pin uses the hub binding version: $line" >&2
      exit 1
      ;;
  esac
done < "$work/pin-lines"

grep -F 'AGENT_PIN=' "$work/n" > /dev/null || { echo "FAIL: dry-run does not pass AGENT_PIN" >&2; cat "$work/n" >&2; exit 1; }
echo "pinned-agent-e2e args OK: hub binds \$AGENT_VERSION ($bound), pin downloads $pin"
