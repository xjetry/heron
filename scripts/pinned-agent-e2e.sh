#!/bin/sh
# 用 AGENT_PIN 指向的已发布 agent 包跑端到端。hub 仍是当前源码构建；绑定版本由构建注入，
# 清单里的 tag 只用来核对 agent 自己报上来的版本。
set -eu
[ "$#" -gt 0 ] || { echo "usage: $0 IMAGE=EXPECTED_OS [...]" >&2; exit 1; }
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
trap 'exit 1' INT TERM HUP
"$root/scripts/pinned-agent-download.sh" "$work/agents"
E2E_AGENT_BIN_DIR="$work/agents"
E2E_AGENT_VERSION=$(jq -r .tag "$AGENT_PIN")
export E2E_AGENT_BIN_DIR E2E_AGENT_VERSION
for pair in "$@"; do
  case "$pair" in
    *=*) image=${pair%%=*}; os=${pair#*=} ;;
    *) echo "FAIL: expected IMAGE=EXPECTED_OS, got $pair" >&2; exit 1 ;;
  esac
  AGENT_IMAGE="$image" EXPECT_OS="$os" "$root/scripts/e2e.sh"
done
echo "pinned published agent OK: $E2E_AGENT_VERSION"
