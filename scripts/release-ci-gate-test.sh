#!/bin/sh
# 发布放行判定的回归检查（make script-test 调用）：release-ci-gate.sh 在 gh 替身上逐例运行，不访问网络。替身按调用
# 次序回放 resp.<n>（--jq 过滤之后的行：<run id> <status> <conclusion>；超出序列时重复最后一份），内容为 ERROR 时以 1
# 退出，模拟网络或权限错误；每次调用的参数记进 gh-calls。--jq 表达式与过滤参数在真实 API 上的语义不由替身承载。
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
bad() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

mkdir "$work/bin"
cat > "$work/bin/gh" <<EOF
#!/bin/sh
n=\$(( \$(cat "$work/count") + 1 ))
echo "\$n" > "$work/count"
printf '%s\n' "\$*" >> "$work/gh-calls"
# 每例最多需要 3 次调用。超过 20 次说明被测脚本在不该等的地方一直等或一直重试：回一次失败的 run 让它退出，
# 以调用次数不符报出，而不是挂住整个测试。
if [ "\$n" -gt 20 ]; then echo "99 completed failure"; exit 0; fi
f="$work/resp.\$n"
[ -f "\$f" ] || f=\$(ls "$work"/resp.* | sort -t. -k2 -n | tail -n 1)
if [ "\$(cat "\$f")" = ERROR ]; then echo "gh: HTTP 403" >&2; exit 1; fi
cat "\$f"
EOF
chmod +x "$work/bin/gh"

sha=0123456789abcdef0123456789abcdef01234567
# gate WAIT APPEAR RESP...：每份 RESP 依次作为第 1、2… 次调用的输出（"" 表示一次 run 都没有）。退出码记进 rc，
# 标准输出与错误输出记进 out，调用次数记进 calls。
gate() {
  wait_max=$1 appear_max=$2
  shift 2
  rm -f "$work"/resp.* "$work/gh-calls"
  echo 0 > "$work/count"
  i=0
  for r in "$@"; do
    i=$((i + 1))
    if [ -n "$r" ]; then printf '%s\n' "$r" > "$work/resp.$i"; else : > "$work/resp.$i"; fi
  done
  rc=0
  PATH="$work/bin:$PATH" RELEASE_CI_GATE_WAIT=$wait_max RELEASE_CI_GATE_APPEAR=$appear_max RELEASE_CI_GATE_INTERVAL=0 \
    "$root/scripts/release-ci-gate.sh" xjetry/heron "$sha" > "$work/out" 2>&1 || rc=$?
  calls=$(cat "$work/count")
}
expect() {
  # expect NAME RC CALLS [TEXT]：退出码、调用次数，以及输出里应当出现的文字。
  [ "$rc" = "$2" ] || { bad "$1: want exit $2, got $rc: $(cat "$work/out")"; return; }
  [ "$calls" = "$3" ] || bad "$1: want $3 gh call(s), got $calls"
  [ -z "${4:-}" ] || grep -qF -- "$4" "$work/out" || bad "$1: output lacks '$4': $(cat "$work/out")"
}

gate 300 300 "11 completed success"
expect "success" 0 1 "run 11 succeeded"
# 查询的是 ci.yml 在这个提交上、push 到 main 产生的 run。
call=$(head -n 1 "$work/gh-calls")
for want in "api -X GET repos/xjetry/heron/actions/workflows/ci.yml/runs " "-f head_sha=$sha " "-f event=push " "-f branch=main " "--jq "; do
  case $call in *"$want"*) ;; *) bad "gh call lacks '$want': $call" ;; esac
done

gate 300 300 "11 in_progress none" "11 completed success"
expect "in progress then success" 0 2 "run 11 succeeded"

gate 300 300 "" "12 queued none" "12 completed success"
expect "appears, runs, succeeds" 0 3 "run 12 succeeded"

gate 300 300 "13 completed failure"
expect "failure" 1 1 "13 completed failure"

gate 300 300 "14 completed cancelled"
expect "cancelled" 1 1 "14 completed cancelled"

# 同一提交被推到 main 两次时有两次 run，其中一次通过即可：两次检出的是同一棵树。
gate 300 300 "15 completed failure
16 completed success"
expect "one of two runs succeeded" 0 1 "run 16 succeeded"

gate 300 300 "17 in_progress none
18 completed success"
expect "one done with success, one still running" 0 1 "run 18 succeeded"

gate 300 0 ""
expect "no run within the appear window" 1 1 "no ci.yml run"

gate 0 300 "19 in_progress none"
expect "still running at the deadline" 1 1 "still running"

gate 300 300 ERROR
expect "gh error" 1 1 "gh: HTTP 403"
! grep -q succeeded "$work/out" || bad "gh error: output claims success: $(cat "$work/out")"

# gh 先出错、之后会成功也不重试：放行只能来自一次读到的 success，出错就整个失败、由人重跑。
gate 300 300 ERROR "20 completed success"
expect "gh error is not retried" 1 1 "gh: HTTP 403"

rc=0
PATH="$work/bin:$PATH" "$root/scripts/release-ci-gate.sh" xjetry/heron > "$work/out" 2>&1 || rc=$?
[ "$rc" = 2 ] || bad "missing argument: want exit 2, got $rc"

if [ "$failures" != 0 ]; then
  echo "release ci gate: $failures failure(s)" >&2
  exit 1
fi
echo "release ci gate ok"
