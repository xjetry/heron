#!/bin/sh
# 发布凭主干 CI 的结论放行：tag 指向的提交必须已经以 push 到 main 的方式跑过 ci.yml 并通过。release.yml 因此不再重跑
# make ci 的测试、脚本与前端门禁：同一个提交就是同一棵树，ci.yml 在它上面跑的是 make ci 的超集（另有 web-e2e、e2e、
# 镜像冒烟与两格 macOS）；这些门禁只读这棵树与固定的工具链，那次通过就是它们的凭据。
# 判定（ci.yml 的 run，经 GitHub API 按 head_sha、event=push、branch=main 过滤）：
# - 有一次 run 的结论是 success：通过。同一提交被推到 main 不止一次时任一次通过即可；重跑失败 job 后转绿的 run，
#   API 给的是最后一次尝试的结论。
# - 还有 run 没有完成：等，上限 RELEASE_CI_GATE_WAIT 秒（默认 3000：ci.yml 的 ci job 超时 45 分钟，加上排队）。
# - 一次 run 都没有：等它出现，上限 RELEASE_CI_GATE_APPEAR 秒（默认 300：推送后 run 通常几秒内建出）。仍然没有，说明
#   这个提交没有推到 main，失败。
# - 全部完成且没有 success：失败，列出每次 run 的结论。
# gh 出错（网络、权限）不重试、照常失败：放行只能来自一次读到的 success，读不到就不能当作通过。
set -eu
[ "$#" = 2 ] || { echo "usage: $0 OWNER/REPO COMMIT_SHA" >&2; exit 2; }
repo=$1
sha=$2
wait_max=${RELEASE_CI_GATE_WAIT:-3000}
appear_max=${RELEASE_CI_GATE_APPEAR:-300}
interval=${RELEASE_CI_GATE_INTERVAL:-30}
start=$(date +%s)
while :; do
  # 每行 <run id> <status> <conclusion>；没有完成的 run 的 conclusion 是 null，写成 none。
  runs=$(gh api -X GET "repos/$repo/actions/workflows/ci.yml/runs" -f head_sha="$sha" -f event=push -f branch=main \
    --jq '.workflow_runs[] | "\(.id) \(.status) \(.conclusion // "none")"') || {
    echo "release-ci-gate: cannot list ci.yml runs for $sha" >&2
    exit 1
  }
  elapsed=$(($(date +%s) - start))
  if [ -z "$runs" ]; then
    if [ "$elapsed" -ge "$appear_max" ]; then
      echo "release-ci-gate: no ci.yml run for a push of $sha to main after ${elapsed}s; push the tagged commit to main, wait for ci.yml, then rerun this workflow" >&2
      exit 1
    fi
  else
    success=$(printf '%s\n' "$runs" | awk '$2 == "completed" && $3 == "success" { print $1; exit }')
    if [ -n "$success" ]; then
      echo "release-ci-gate: ci.yml run $success succeeded on $sha"
      exit 0
    fi
    if [ -z "$(printf '%s\n' "$runs" | awk '$2 != "completed"')" ]; then
      echo "release-ci-gate: ci.yml did not succeed on $sha (run status conclusion):" >&2
      printf '%s\n' "$runs" >&2
      exit 1
    fi
    if [ "$elapsed" -ge "$wait_max" ]; then
      echo "release-ci-gate: ci.yml still running on $sha after ${elapsed}s (run status conclusion):" >&2
      printf '%s\n' "$runs" >&2
      exit 1
    fi
  fi
  echo "release-ci-gate: waiting for ci.yml on $sha (${elapsed}s): $(printf '%s' "$runs" | tr '\n' ';')"
  sleep "$interval"
done
