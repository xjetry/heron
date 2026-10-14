#!/bin/sh
# 首发判定的回归检查（make script-test 调用）：在临时仓库里构造历史与 tag，核对 release-breaking-base.sh 给出的基线
# 与 none 的取舍，以及 buf-breaking.sh 只在 HEAD 之前没有发布时接受 none。buf 换成只记录参数的替身，不访问网络。
set -eu
# 外层 make 或 git hook 留下的这几个变量会让临时仓库里的 git 命令改去操作别的仓库。
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0
bad() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

new_repo() { git init -q "$1"; }
commit() {
  git -C "$1" -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m "$2"
  git -C "$1" rev-parse HEAD
}
annotated_tag() { git -C "$1" -c user.name=t -c user.email=t@example.invalid tag -a "$2" -m "$2" "$3"; }
base() { (cd "$1" && "$root/scripts/release-breaking-base.sh" "$2"); }

# 线性历史，根提交就是第一个发布。
a=$work/a
new_repo "$a"
c1=$(commit "$a" one)
git -C "$a" tag v1.0.0 "$c1"
got=$(base "$a" v1.0.0) || got="exit $?"
[ "$got" = none ] || bad "root commit tagged v1.0.0: want none, got $got"
c2=$(commit "$a" two)
annotated_tag "$a" v1.0.1 "$c2"
got=$(base "$a" v1.0.1) || got="exit $?"
[ "$got" = v1.0.0 ] || bad "v1.0.1 after v1.0.0: want v1.0.0, got $got"
c3=$(commit "$a" three)
git -C "$a" tag not-a-release "$c3"
c4=$(commit "$a" four)
git -C "$a" tag v1.1.0 "$c4"
got=$(base "$a" v1.1.0) || got="exit $?"
[ "$got" = v1.0.1 ] || bad "v1.1.0 with a non-release tag between: want v1.0.1, got $got"

# 第一个发布之前有未打 tag 的提交。
b=$work/b
new_repo "$b"
commit "$b" pre > /dev/null
d2=$(commit "$b" first)
annotated_tag "$b" v1.0.0 "$d2"
got=$(base "$b" v1.0.0) || got="exit $?"
[ "$got" = none ] || bad "first release after untagged commits: want none, got $got"

# 错误不能被当作首发。
if out=$(base "$a" v9.9.9 2>&1); then bad "unknown version accepted: $out"; fi
if out=$(cd "$work" && "$root/scripts/release-breaking-base.sh" v1.0.0 2>&1); then bad "outside a repository accepted: $out"; fi

# buf-breaking.sh：替身 buf 只记录参数。
mkdir "$work/bin"
printf '#!/bin/sh\necho "$*" >> "%s/buf-calls"\n' "$work" > "$work/bin/buf"
chmod +x "$work/bin/buf"
breaking() { (cd "$1" && PATH="$work/bin:$PATH" "$root/scripts/buf-breaking.sh" "$2"); }
: > "$work/buf-calls"
if out=$(breaking "$a" none 2>&1); then bad "none accepted at a HEAD that follows releases: $out"; fi
[ ! -s "$work/buf-calls" ] || bad "rejected none still ran buf: $(cat "$work/buf-calls")"
git -C "$b" checkout -q v1.0.0
breaking "$b" none > /dev/null || bad "none rejected at the first release"
[ ! -s "$work/buf-calls" ] || bad "skipped check still ran buf: $(cat "$work/buf-calls")"
breaking "$a" v1.0.0 > /dev/null || bad "buf-breaking.sh v1.0.0 failed"
[ "$(cat "$work/buf-calls")" = "breaking --against .git#ref=v1.0.0" ] || bad "buf arguments: $(cat "$work/buf-calls")"

[ "$failures" = 0 ] || exit 1
echo "release-breaking-base: ok"
