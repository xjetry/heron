#!/bin/sh
# 版本号守卫与发布规则的回归检查（make ci 调用）：Makefile 的 make 层守卫、check_version、发布目标的
# 组成与打包输入登记、RELEASE_CHANNEL 与推送命令里 latest 的取舍。只跑 make 的检查与 -n 展开，
# 不构建、不碰 docker；两个发布目标的资产集合由 scripts/release-assets-test.sh 用替身执行核对。
set -eu
make_path=$(command -v "${MAKE:-make}") || { echo "FAIL: make not found" >&2; exit 1; }
# 用例自己给出 VERSION；从外层 make 继承的 MAKEFLAGS（含命令行变量）与环境变量会顶替用例的取值。
unset MAKEFLAGS MFLAGS MAKELEVEL VERSION SMOKE_PLATFORM
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

# make 只在这个 PATH 下运行：里面只有只读的文本工具（守卫可能用到的 tr、grep、sed，以及 echo——make 对
# 不含 shell 元字符的配方行直接 exec，不经 shell 的内建），加上 docker、go、pnpm、gh 的绊线。被拒的用例
# 跑的是真目标，守卫一旦失效，配方会接着构建乃至推送；在这个 PATH 下其余命令要么找不到，要么只在
# 绊线文件里留下记录，什么都不会真的发生。
mkdir "$work/bin"
# echo 与 printf 在 sh 里是内建，但 make 对不含 shell 元字符的配方行（如片段里的 @printf）不经 shell 直接
# exec，command -v 给不出路径，按系统目录找可执行文件。printf 只写标准输出，与 tr、grep、sed 同为只读文本工具。
for tool in tr grep sed echo printf; do
  for dir in /bin /usr/bin; do
    if [ -x "$dir/$tool" ]; then
      ln -s "$dir/$tool" "$work/bin/$tool"
      break
    fi
  done
  [ -e "$work/bin/$tool" ] || { echo "FAIL: no $tool in /bin or /usr/bin" >&2; exit 1; }
done
for tool in docker go pnpm gh; do
  printf '#!/bin/sh\necho "%s $*" >> "%s/tripwire"\nexit 1\n' "$tool" "$work" > "$work/bin/$tool"
  chmod +x "$work/bin/$tool"
done
# 内层 make 的 $(MAKE) 必须是绝对路径：配方里的递归行（$(MAKE) web 等）即使在 -n 下也会真的执行，而它们
# 跑在只有文本工具的 PATH 里。make 默认把 $(MAKE) 取自自己的 argv[0]，但同名环境变量的优先级高于这个默认值
# （origin 为 environment 与 default 的关系），外层 Makefile 传进来的 MAKE 在按 PATH 启动 make 的系统上就是
# 裸的 make；macOS 的 make 垫片重新 exec 时会把 argv[0] 换成绝对路径，掩盖了这一点。这里显式传绝对路径。
MAKE() {
  env PATH="$work/bin" MAKE="$make_path" "$make_path" "$@"
}

bad() {
  echo "FAIL: $*" >&2
  if [ -s "$work/out" ]; then sed 's/^/    /' "$work/out" >&2; fi
  failures=$((failures + 1))
}

# 每个消费 VERSION 的目标都必须以 check_version 开头：配方其余部分直接展开 $(VERSION)。release 换成了
# release-full / release-hub-only / release-kind，rejects 的各条经 $targets 对三者照旧成立。
# 镜像组（deploy/image.mk）由 IMAGE_GROUPS 列出、每组展开同一组目标；组的集合在这里钉住，加组要同时改这里与下面
# 按组钉住的仓库名。
# shellcheck disable=SC2016 # 写进 makefile 的 make 引用
printf 'print-image-groups:\n\t@echo $(IMAGE_GROUPS)\n' > "$work/eval.mk"
image_groups=$(MAKE -s -f Makefile -f "$work/eval.mk" print-image-groups 2>&1) || bad "make print-image-groups: $image_groups"
# shellcheck disable=SC2086 # 组名按词拆开，逐行排序
[ "$(printf '%s\n' $image_groups | sort | tr '\n' ' ')" = 'agent hub ' ] ||
  bad "IMAGE_GROUPS is '$image_groups', want the hub and agent groups"
image_targets=docker
for g in $image_groups; do
  image_targets="$image_targets docker-$g docker-$g-smoke docker-$g-push docker-$g-readback docker-$g-promote"
done
targets="release-full release-hub-only release-manifest release-readback release-kind release-goflags hub-goflags $image_targets release-channel"
for t in $targets; do
  MAKE -n "$t" VERSION=v1.0.0 > "$work/out" 2>&1 || { bad "make -n $t exited non-zero"; continue; }
  # shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$VERSION 按字面出现
  case $(sed -n 1p "$work/out") in
    'if [ -z "$VERSION" ]; then echo "VERSION is required'*) ;;
    *) bad "$t does not start with check_version" ;;
  esac
done

# rejects TARGET VALUE WANT：退出码非 0，输出含 WANT，且第一行就是守卫的报错（shell 层以 "VERSION " 开头，
# make 层是 "*** VERSION"）。要看第一行：守卫是配方第一行、打印后立即退出，它之前不会有任何输出；守卫被删
# 时第一行是下一条配方的回显；配方若递归调用 $(MAKE) 去跑另一个带守卫的目标，子 make 会再打印同样的报错，
# 输出里仍有 WANT 而守卫已不在第一行，所以只凭"输出含 WANT"钉不住守卫；release-channel 的下一条是静默的 echo，删守卫后它以 0 退出，由 accepted
# 分支拦下。带换行的值会把守卫的报错拆成两行，WANT 因此不限定在第一行。
# 绊线文件为空证明 docker、go、pnpm、gh 一次都没被调用。
rejects() {
  rc=0
  MAKE "$1" "VERSION=$2" > "$work/out" 2>&1 || rc=$?
  if [ "$rc" = 0 ]; then
    bad "make $1 accepted VERSION '$2'"
  elif ! grep -qF -- "$3" "$work/out"; then
    bad "make $1 with VERSION '$2' did not fail on the version check (want '$3')"
  elif ! sed -n 1p "$work/out" | grep -q '^VERSION \|\*\*\* VERSION'; then
    bad "make $1 with VERSION '$2' ran past the version check before failing"
  fi
}

shape="cannot be an image tag"
long=$(printf 'a%.0s' $(seq 129))
nl='v1.0
x'
for t in $targets; do
  rejects "$t" '' 'VERSION is required, e.g. VERSION=v0.1.0'
  rejects "$t" "v1'x'" "$shape"
  rejects "$t" "$nl" "$shape"
  rejects "$t" 'v1.0+meta' "$shape"
  rejects "$t" '-v1' "$shape"
  rejects "$t" "$long" "$shape"
  # make 层守卫：原文里的 $ 在解析 Makefile 时就被拒绝，函数一次也没有执行。
  rejects "$t" "v1\$(shell echo expanded > $work/expanded)" "contains '\$'"
  # shellcheck disable=SC2016 # 原样交给 make 的字面值，由 make 层守卫拒绝
  rejects "$t" 'v1$$(id -un)' "contains '\$'"
  # shellcheck disable=SC2016
  rejects "$t" 'v1.0$(e)' "contains '\$'"
done
[ ! -e "$work/expanded" ] || bad "a \$(shell …) in VERSION was executed"
if [ -s "$work/tripwire" ]; then
  cp "$work/tripwire" "$work/out"
  bad "recipes ran past the version check"
fi

# AGENT_VERSION 的 make 层守卫：与 VERSION 同理，命令行值里的 $ 在解析 Makefile 时就被拒绝，
# $(shell …) 一次也没有执行。
for t in release-full release-hub-only release-kind; do
  # shellcheck disable=SC2016 # 交给 make 的字面值，由 make 层守卫拒绝
  MAKE "$t" VERSION=v1.0.0 'AGENT_VERSION=v1$(shell touch '"$work"'/expanded-agent)' > "$work/out" 2>&1 && bad "make $t accepted a \$ in AGENT_VERSION"
  grep -qF "contains '\$'" "$work/out" || bad "make $t did not reject a \$ in AGENT_VERSION at parse time"
  [ ! -e "$work/expanded-agent" ] || bad "a \$(shell …) in AGENT_VERSION was executed"
done

# 发布目标的组成。-n 只核对不经 shell 循环的行：循环体里的 $$arch 在 -n 下原样打印，按具体架构的
# 文件名去找必然找不到。完整 release 不碰 agentinputs/boundagent；只发 hub 的门禁先于任何构建（第一条 pnpm）。
MAKE -n release-hub-only VERSION=v1.2.4 AGENT_VERSION=v1.2.3 > "$work/hub-only-n" 2>&1 || bad "make -n release-hub-only failed"
MAKE -n release-full VERSION=v1.2.3 AGENT_VERSION=v1.2.3 > "$work/full-n" 2>&1 || bad "make -n release-full failed"
# shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$AGENT_VERSION 按字面出现
grep -qF 'go run ./scripts/agentinputs -base "$AGENT_VERSION"' "$work/hub-only-n" || bad "release-hub-only does not gate on scripts/agentinputs"
grep -qF 'go run ./scripts/boundagent fetch' "$work/hub-only-n" || bad "release-hub-only does not fetch the bound agent's installers"
gate_line=$(awk '/agentinputs/ { print NR; exit }' "$work/hub-only-n")
first_pnpm=$(awk '/pnpm/ { print NR; exit }' "$work/hub-only-n")
case $gate_line in '' | *[!0-9]*) bad "no agentinputs line in release-hub-only's -n output" ;; esac
case $first_pnpm in '' | *[!0-9]*) bad "no pnpm line in release-hub-only's -n output" ;; esac
[ "$gate_line" -lt "$first_pnpm" ] || bad "release-hub-only runs its gate after the web build"
if grep -q 'agentinputs\|boundagent' "$work/full-n"; then
  cp "$work/full-n" "$work/out"
  bad "release-full runs a hub-only step"
fi
# 只发 hub 不碰 agent 镜像组：发布目标本身不构建镜像，这里钉住它没有引用 agent 组的任何目标、Dockerfile 或清单。
if grep -q 'docker-agent\|Dockerfile\.agent\|MANIFEST=agent\|heron-agent:' "$work/hub-only-n"; then
  cp "$work/hub-only-n" "$work/out"
  bad "release-hub-only references the agent image group"
fi
MAKE -n release-kind VERSION=v1.2.3 > "$work/out" 2>&1 || bad "make -n release-kind failed"
# shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$VERSION 按字面出现
grep -qF 'go run ./scripts/releasekind -version "$VERSION" -agent "$AGENT_VERSION"' "$work/out" || bad "release-kind does not print the releasekind call"
# 发布产物去掉符号表与 DWARF：完整 release、只发 hub 与镜像里的每一条 go build 都在 -ldflags 里带 -s 与
# -w；只有 ./scripts/ 下的工具（镜像目标编的 checkimage）除外，它们在构建机上运行，不发布。排除按包列出的工具，其余
# 一律核对：新加的构建默认受查，不因换了包路径的写法漏掉。三条路径的参数都由 deploy/agent.mk 的 RELEASE_LDFLAGS
# 组成；核对落在 -n 展开后的命令上，绕开它另写的构建、或从某个参数变量里拿掉它，都在这里失败。一行里可能有多条
# go build（循环体），逐条切开：参数取到 -o 之前，包取到命令结束。
MAKE -n docker VERSION=v1.2.3 > "$work/docker-n" 2>&1 || bad "make -n docker failed"
for f in full-n hub-only-n docker-n; do
  unstripped=$(awk '{
    n = split($0, parts, /go build /)
    for (i = 2; i <= n; i++) {
      flags = parts[i]; sub(/ -o .*/, "", flags)
      target = parts[i]; sub(/[;&|].*/, "", target)
      if (target ~ / \.\/scripts\//) continue
      builds++
      lf = ""
      if (match(flags, /-ldflags "[^"]*"/)) lf = " " substr(flags, RSTART + 10, RLENGTH - 11) " "
      if (lf !~ / -s / || lf !~ / -w /) print "go build " flags
    }
  } END { if (builds == 0) print "(no release go build at all)" }' "$work/$f")
  [ -z "$unstripped" ] || bad "${f%-n} builds a release binary without -s -w: $unstripped"
done
# make docker 构建两个镜像组：hub 与 agent 的二进制都要在上面的核对范围里，否则"每条 go build 都带 -s -w"可能只
# 看到了一个组。
for pkg in ./cmd/hub ./cmd/agent; do
  awk -v pkg="$pkg" '{
    n = split($0, parts, /go build /)
    for (i = 2; i <= n; i++) { cmd = parts[i]; sub(/[;&|].*/, "", cmd); sub(/ +$/, "", cmd); if (substr(cmd, length(cmd) - length(pkg)) == " " pkg) found = 1 }
  } END { exit !found }' "$work/docker-n" || bad "make -n docker does not build $pkg for an image"
done
# scripts/wirebench/run.sh 经 release-goflags 与 hub-goflags 取参数：两者打印的必须就是发布配方里 go build 拿到的那段。
# 完整 release 的各个循环展开在同一行里，按条切开后逐条比对参数与包，不按行匹配。
for pair in release-goflags=./cmd/agent hub-goflags=./cmd/hub; do
  flags=$(MAKE --no-print-directory -s "${pair%%=*}" VERSION=v1.2.3 2>&1) || bad "make ${pair%%=*}: $flags"
  awk -v flags="$flags" -v pkg="${pair#*=}" '{
    n = split($0, parts, /go build /)
    for (i = 2; i <= n; i++) {
      cmd = parts[i]; sub(/[;&|].*/, "", cmd); sub(/ +$/, "", cmd)
      if (index(cmd, flags " -o ") == 1 && substr(cmd, length(cmd) - length(pkg)) == " " pkg) found = 1
    }
  } END { exit !found }' "$work/full-n" ||
    bad "make ${pair%%=*} prints '$flags', which release-full does not pass to go build for ${pair#*=}"
done

# 每一次 shellcheck 调用都经 Makefile 固定的 $(SHELLCHECK)：不同版本的诊断不同，PATH 上的 shellcheck 随机器而变。
# 判定落在 make -n 展开后的命令上，而不是 Makefile 源文本：经变量或宏拼出的调用也照得到，注释与变量定义不会误伤。
# 覆盖面是两个 makefile 里 .PHONY 列出的每个目标（连同它们的前置条件），只排除两个：script-test 的配方行引用
# $(MAKE)，make 在 -n 下也会整行执行它，等于在这里递归运行本测试；ci 的前置条件含 script-test，它自己的配方只有
# 生成目录的检查，其余前置条件都已逐个在列。命令行里任何一个以 shellcheck 为名的词（裸名或以 /shellcheck 结尾的
# 路径）都必须恰好是 $(SHELLCHECK)；先把 shell 的分隔与引号字符换成空格，cmd && shellcheck、$(shellcheck …) 这类
# 写法都拆得出来。
sc_path=$(MAKE -s --no-print-directory shellcheck-path 2>&1) || bad "make shellcheck-path: $sc_path"
case $sc_path in
  build/tools/*/shellcheck) ;;
  *) bad "make shellcheck-path printed '$sc_path'" ;;
esac
# unpinned_shellcheck FILE：打印 FILE 里调用了 $(SHELLCHECK) 以外的 shellcheck 的行。set -f 防止 *.tar.gz 之类的词被展开。
unpinned_shellcheck() {
  (
    set -f
    while IFS= read -r line; do
      for w in $(printf '%s\n' "$line" | tr ";&|()<>\`'\"" '          '); do
        case $w in
          shellcheck | */shellcheck)
            if [ "$w" != "$sc_path" ]; then
              printf '%s\n' "$line"
              break
            fi
            ;;
        esac
      done
    done < "$1"
  )
}
# 镜像组的目标由宏展开，宏里的 .PHONY 行带着 $(1)，不是可调用的目标名：跳过带 $ 的词，组的目标从 image_targets 补上。
phony=$(sed -n 's/^\.PHONY:[[:space:]]*//p' Makefile deploy/agent.mk deploy/image.mk | tr ' ' '\n' | grep -v '\$' | tr '\n' ' ')
[ -n "$phony" ] || bad "no .PHONY targets found in Makefile, deploy/agent.mk and deploy/image.mk"
checked=
for t in $phony $image_targets; do
  case $t in script-test | ci) continue ;; esac
  MAKE -n "$t" VERSION=v1.2.3 AGENT_VERSION=v1.2.3 > "$work/out" 2>&1 || { bad "make -n $t failed"; continue; }
  bypass=$(unpinned_shellcheck "$work/out")
  [ -z "$bypass" ] || bad "make -n $t runs a shellcheck other than $sc_path:
$(printf '%s\n' "$bypass" | sed 's/^/    /')"
  checked="$checked $t"
done
# 冒烟：lint 与两个发布目标确实调用了固定路径的 shellcheck，否则上面的判定可能什么都没看到。
for t in lint release-full release-hub-only; do
  case " $checked " in
    *" $t "*) ;;
    *) bad "the shellcheck pin check did not cover $t" ;;
  esac
  MAKE -n "$t" VERSION=v1.2.3 AGENT_VERSION=v1.2.3 > "$work/out" 2>&1 || bad "make -n $t failed"
  grep -qF "$sc_path -s sh " "$work/out" || bad "make -n $t does not run $sc_path"
done

# 以上 -n 命令里会被执行的只有 $(MAKE) web 与 $(MAKE) 安装 shellcheck 两行递归 make，它们也只打印：到这里绊线文件仍为空。
if [ -s "$work/tripwire" ]; then
  cp "$work/tripwire" "$work/out"
  bad "a -n run executed a tripwire command"
fi

# 打包输入登记完整：deploy/agent.mk 里出现的每个 deploy/ 路径都登记在 AGENT_BUNDLE_FILES。只发 hub 的门禁
# （scripts/agentinputs）把这个片段本身当作 agent 组的输入，写死在配方里的路径它看不见。
MAKE -s agent-bundle-inputs > "$work/bundle" 2>&1 || { cp "$work/bundle" "$work/out"; bad "make agent-bundle-inputs failed"; }
# shellcheck disable=SC2013 # 路径不含空白，逐个比对
for p in $(grep -o 'deploy/[A-Za-z0-9_./-][A-Za-z0-9_./-]*' deploy/agent.mk | sort -u); do
  grep -qxF "$p" "$work/bundle" || bad "deploy/agent.mk references unregistered $p"
done
# 登记的每一项都是仓库里存在的文件：拼错的路径在门禁里两侧都"不存在"，比较结果恒为未变。
while IFS= read -r p; do
  [ -f "$p" ] || bad "AGENT_BUNDLE_FILES lists $p, which is not a file"
done < "$work/bundle"
# 镜像组的输入裁决（deploy/image.mk 开头与 IMAGE_UNBUNDLED 的说明）：deploy/image.mk 里引用的每个仓库路径要么登记进
# AGENT_BUNDLE_FILES（决定镜像字节），要么列在 IMAGE_UNBUNDLED（只核对或搬运结果）；新增引用而不裁决即失败。
# shellcheck disable=SC2016 # 写进 makefile 的 make 引用
printf 'print-image-unbundled:\n\t@printf "%%s\\n" $(IMAGE_UNBUNDLED)\n' > "$work/eval.mk"
MAKE -s -f Makefile -f "$work/eval.mk" print-image-unbundled > "$work/unbundled" 2>&1 ||
  { cp "$work/unbundled" "$work/out"; bad "make print-image-unbundled failed"; }
# shellcheck disable=SC2013 # 路径不含空白，逐个比对
# 只看非注释行：注释里提到的脚本（测试、hub 的冒烟）不是配方的输入。
for p in $(grep -v '^[[:space:]]*#' deploy/image.mk | grep -o '\(deploy\|scripts\)/[A-Za-z0-9_./-]*[A-Za-z0-9_-]\|\.dockerignore' | sort -u); do
  grep -qxF "$p" "$work/bundle" || grep -qxF "$p" "$work/unbundled" ||
    bad "deploy/image.mk references $p, which is neither registered in AGENT_BUNDLE_FILES nor listed in IMAGE_UNBUNDLED"
done
while IFS= read -r p; do
  ! grep -qxF "$p" "$work/bundle" || bad "$p is both registered in AGENT_BUNDLE_FILES and listed in IMAGE_UNBUNDLED"
  [ -e "$p" ] || bad "IMAGE_UNBUNDLED lists $p, which does not exist"
done < "$work/unbundled"
# 裁决本身钉住：决定 agent 镜像字节的四项登记，hub 专属的四项不登记（只发 hub 的 release 改它们仍然放行）。
for p in deploy/image.mk deploy/Dockerfile.agent .dockerignore scripts/docker-builder.sh; do
  grep -qxF "$p" "$work/bundle" || bad "$p decides the agent image's bytes but is not registered in AGENT_BUNDLE_FILES"
done
for p in Dockerfile Makefile scripts/docker-smoke.sh scripts/checkimage; do
  ! grep -qxF "$p" "$work/bundle" || bad "$p is hub-only but registered in AGENT_BUNDLE_FILES"
done

# 两个发布目标只碰登记过的文件：-n 输出里出现的每个 deploy/ 路径（make 已把变量展开成字面路径）都属于
# agent 组的登记清单，或 hub 组的 deploy/systemd/heron-hub.service 与 deploy/install-hub.sh。
for out in "$work/full-n" "$work/hub-only-n"; do
  # shellcheck disable=SC2013 # 路径不含空白，逐个比对
  for p in $(grep -o 'deploy/[A-Za-z0-9_./-][A-Za-z0-9_./-]*' "$out" | sort -u); do
    case $p in
      deploy/systemd/heron-hub.service | deploy/install-hub.sh) continue ;;
    esac
    grep -qxF "$p" "$work/bundle" || bad "release recipes reference unregistered $p"
  done
done

# 主 Makefile 不 export 影响构建的变量：export 会传进每一条配方的环境，绕开配方的 PATH 约束；片段的
# CGO_ENABLED=0 只属于它自己，其余构建参数都经显式 env 或 gflags 进入命令。主 Makefile 唯一的 export 是
# AGENT_VERSION（配方的运行时输入，不是构建参数）。
exports=$(grep -E '^[[:space:]]*export([[:space:]]|$)|\.EXPORT_ALL_VARIABLES' Makefile || true)
if [ "$(printf '%s\n' "$exports" | grep -c .)" != 1 ] || [ "$(printf '%s\n' "$exports" | sed -n 1p)" != 'export AGENT_VERSION' ]; then
  printf '%s\n' "$exports" > "$work/out"
  bad "main Makefile exports more than AGENT_VERSION"
fi

# 默认目标不变：不带目标的 make 一直做的是执行第一个普通目标（基点 Makefile 的是 web-install），片段的
# include 位置不能改变它。
default_out=$(MAKE -n 2>&1) || bad "make -n without a target failed"
webinstall_out=$(MAKE -n web-install 2>&1) || bad "make -n web-install failed"
[ "$default_out" = "$webinstall_out" ] || bad "the default make target changed (agent.mk must include after the first regular target)"

# accepts VALUE CHANNEL：经 check_version 放行，release-channel 打印判定。
accepts() {
  rc=0
  MAKE -s release-channel "VERSION=$1" > "$work/out" 2>&1 || rc=$?
  if [ "$rc" != 0 ] || [ "$(cat "$work/out")" != "$2" ]; then
    bad "make release-channel VERSION=$1: exit $rc, want '$2'"
  fi
}
accepts v1.0.0 stable
accepts v1.0.0-rc.1 prerelease

# RELEASE_CHANNEL 本身的规则：去掉构建元数据后仍含 - 才是预发布。带 + 的值过不了 check_version，
# 这里绕开目标直接求值。
for pair in v1.2.3=stable v1.2.3-rc.1=prerelease v1.2.3+build-5=stable v1.2.3-rc.1+b.2=prerelease; do
  # shellcheck disable=SC2016 # 写进 makefile 的 make 引用
  printf 'print-release-channel:\n\t@echo $(RELEASE_CHANNEL)\n' > "$work/eval.mk"
  got=$(MAKE -s -f Makefile -f "$work/eval.mk" print-release-channel "VERSION=${pair%%=*}" 2>&1) || got="exit $?: $got"
  [ "$got" = "${pair#*=}" ] || bad "RELEASE_CHANNEL for ${pair%%=*}: got '$got', want '${pair#*=}'"
done

# release.yml 的 image、readback、promote 三个 job 只分别调用各组的 docker-<组>-push、-readback、-promote：这三个
# 目标的配方逐行只能是版本守卫（与 check_version 的展开逐字相同）、readback 的构建器核对与编译只依赖标准库的
# checkimage（GOPROXY=off，依赖由 scripts/checkimage 的测试守着），以及带着参数调用 scripts/docker-image.sh 的对应
# 子命令。递归的 $(MAKE)、前端与 buf、go run / go test、第三方 Go 模块的编译、登录与推送命令，任何一种混进来都会
# 多出一行或改变一行而在这里失败；除版本守卫与 checkimage 那两行逐字比对外，其余行不得含命令分隔与替换，免得在
# 参数里夹带命令。readback 与 promote 的调用行必须同时带着构建记录与回读记录：两份记录的比对在脚本里。
# shellcheck disable=SC2016 # 写进 makefile 的 make 引用
printf 'print-check-version:\n\t$(check_version)\n' > "$work/eval.mk"
check_version_line=$(MAKE -n -f Makefile -f "$work/eval.mk" print-check-version VERSION=v1.2.3 2>&1) ||
  bad "make -n print-check-version: $check_version_line"
image_job_line() {
  [ "$2" = "$check_version_line" ] && return 0
  case $1 in *-readback) ;; *) false ;; esac &&
    [ "$2" = 'mkdir -p build/tools && GOPROXY=off go build -o build/tools/checkimage ./scripts/checkimage' ] && return 0
  # shellcheck disable=SC2016 # 按字面匹配命令替换的开头 $(
  case $2 in
    *';'* | *'&'* | *'|'* | *'`'* | *'$('* | *'<'* | *'>'*) return 1 ;;
    'scripts/docker-builder.sh '*) case $1 in *-readback) ;; *) false ;; esac ;;
    IMAGE_REPO=*' scripts/docker-image.sh '"$3") return 0 ;;
    *) return 1 ;;
  esac
}
# 按组钉住的仓库名与记录位置：每组的构建记录、回读记录与布局都在 IMAGE_ARCHIVE_DIR/<组>/ 下，两组互不混用；
# 只有 agent 组的 readback 带着 hub 组的布局与构建记录（冒烟对着 hub），push 与 promote 都不带。
here=$(pwd -P)
group_repo() {
  case $1 in
    hub) echo ghcr.io/xjetry/heron-hub ;;
    agent) echo ghcr.io/xjetry/heron-agent ;;
  esac
}
# call_var LINE VAR：LINE 里 VAR 最后一次赋值的值（同一行里后出现的赋值生效，sed 的 .* 是贪婪的）；没有赋值时打印 <unset>。
call_var() {
  case " $1" in
    *" $2="*) printf '%s\n' " $1" | sed "s/.* $2=\([^ ]*\).*/\1/" ;;
    *) echo '<unset>' ;;
  esac
}
for g in $image_groups; do
  for pair in push=push readback=verify promote=promote smoke=smoke; do
    t=docker-$g-${pair%%=*}
    MAKE -n "$t" VERSION=v1.2.3 > "$work/out" 2>&1 || { bad "make -n $t"; continue; }
    call=$(grep "scripts/docker-image.sh ${pair#*=}\$" "$work/out") || { bad "make -n $t does not call scripts/docker-image.sh ${pair#*=}"; continue; }
    for want in "IMAGE_REPO=$(group_repo "$g")" "MANIFEST=$g" "BUILT=build/image-archive/$g/built" "LAYOUT='$here/build/image-archive/$g/layout'"; do
      [ "$(call_var "$call" "${want%%=*}")" = "${want#*=}" ] || bad "$t passes ${want%%=*}=$(call_var "$call" "${want%%=*}"), want ${want#*=}"
    done
    case $pair in
      readback=* | promote=*)
        [ "$(call_var "$call" RECORD)" = "build/image-archive/$g/readback" ] ||
          bad "$t passes RECORD=$(call_var "$call" RECORD), want build/image-archive/$g/readback"
        ;;
    esac
    # 回读的干净环境检查要知道镜像二进制的构建目录（deploy/image.mk 的 IMAGE_BIN_DIR），漏传时 verify 拒绝运行。
    case $pair in
      readback=*)
        [ "$(call_var "$call" IMAGE_BIN_DIR)" = build/image ] ||
          bad "$t passes IMAGE_BIN_DIR=$(call_var "$call" IMAGE_BIN_DIR), want build/image"
        ;;
    esac
    hub_layout=$(call_var "$call" HUB_LAYOUT)
    hub_built=$(call_var "$call" HUB_BUILT)
    case $g:$pair in
      agent:readback=* | agent:smoke=*)
        [ "$hub_layout" = "'$here/build/image-archive/hub/layout'" ] && [ "$hub_built" = build/image-archive/hub/built ] ||
          bad "$t does not hand the hub group's layout and build record to the smoke (HUB_LAYOUT=$hub_layout HUB_BUILT=$hub_built)"
        ;;
      *)
        [ "$hub_layout" = '<unset>' ] && [ "$hub_built" = '<unset>' ] ||
          bad "$t passes another group's records (HUB_LAYOUT=$hub_layout HUB_BUILT=$hub_built)"
        ;;
    esac
  done
done

for v in v1.2.3 v1.2.3-rc.1; do
  for pair in $(for g in $image_groups; do printf 'docker-%s-push=push docker-%s-readback=verify docker-%s-promote=promote ' "$g" "$g" "$g"; done); do
    t=${pair%%=*}
    MAKE -n "$t" "VERSION=$v" > "$work/out" 2>&1 || { bad "make -n $t VERSION=$v"; continue; }
    [ ! -s "$work/tripwire" ] || bad "make -n $t VERSION=$v ran $(cat "$work/tripwire")"
    grep -q "scripts/docker-image.sh ${pair#*=}\$" "$work/out" || bad "make -n $t VERSION=$v does not call scripts/docker-image.sh ${pair#*=}"
    while IFS= read -r line; do
      image_job_line "$t" "$line" "${pair#*=}" || bad "$t runs a command its release job must not run: $line"
    done < "$work/out"
    case $t in
      *-readback | *-promote)
        call=$(grep "scripts/docker-image.sh ${pair#*=}\$" "$work/out")
        for var in BUILT RECORD; do
          case $(call_var "$call" "$var") in
            '<unset>') bad "make -n $t VERSION=$v does not pass $var to scripts/docker-image.sh" ;;
            '') bad "make -n $t VERSION=$v passes an empty $var to scripts/docker-image.sh" ;;
          esac
        done
        ;;
    esac
  done
done
# docker-promote 拿到的 CHANNEL 与预发布判定一致；latest 只由它按回读过的 digest 移动（scripts/docker-image.sh）。
for g in $image_groups; do
  for pair in v1.2.3=stable v1.2.3-rc.1=prerelease; do
    MAKE -n "docker-$g-promote" "VERSION=${pair%%=*}" > "$work/out" 2>&1 || bad "make -n docker-$g-promote VERSION=${pair%%=*}"
    grep -q "CHANNEL=${pair#*=} " "$work/out" || bad "docker-$g-promote for ${pair%%=*} does not pass CHANNEL=${pair#*=}"
  done
done

# release.yml 里只发 hub 的路径不碰 agent 镜像组：agent 组的镜像只随完整 release 构建、推送、回读与
# promote。按步骤核对：一个步骤若调用的 make 目标展开后处理 agent 组（-n 输出带 MANIFEST=agent 或
# deploy/Dockerfile.agent），或读写 agent 组的回读记录，它就必须只在完整 release 时运行——build job 里条件是
# steps.kind.outputs.kind == 'full'，其余 job 里是 needs.build.outputs.kind == 'full'。用后者的 job 必须在 needs 里
# 列出 build（否则读到空值、agent 组被静默跳过），build 必须把判定作为 kind 输出。hub 组的三个目标在三个 job 里
# 无条件运行：只发 hub 时 hub 镜像照常推送、回读、移动 latest。
# workflow_steps：每个步骤一行 <job>US<if 条件>US<步骤正文>（US 是 \037；去掉注释，续行并成一行）；每个 job 的
# needs 一行 NEEDSUS<job>US<值>。分隔符不能是 tab：tab 是 IFS 空白字符，read 会把连续的 tab 并成一个，没有 if 的
# 步骤的空条件字段就被吞掉、正文挪进条件字段——恰好让无条件的步骤从核对里消失。只认这个文件的缩进形状：job 两格、
# job 的键四格、步骤 "      - " 开头、步骤的键八格。
us=$(printf '\037')
workflow_steps() {
  awk '
    function flush() { if (instep) print job "\037" cond "\037" text; instep = 0; text = ""; cond = "" }
    function add(line) {
      sub(/^ +/, "", line)
      if (line ~ /^#/) return
      if (line ~ /^if: /) { c = line; sub(/^if: /, "", c); cond = c }
      text = text " " line
    }
    /^jobs:/ { injobs = 1; next }
    !injobs { next }
    /^  [A-Za-z0-9_-]+:/ { flush(); job = $1; sub(/:$/, "", job); next }
    /^    needs: / { flush(); v = $0; sub(/^    needs: /, "", v); print "NEEDS\037" job "\037" v; next }
    /^    [^ ]/ { flush(); next }
    /^      - / { flush(); instep = 1; line = $0; sub(/^      - /, "", line); add(line); next }
    instep && /^       / { add($0); next }
    END { flush() }
  ' "$1"
}
workflow_steps .github/workflows/release.yml > "$work/steps"
[ "$(grep -vc '^NEEDS' "$work/steps")" -gt 20 ] || { cp "$work/steps" "$work/out"; bad "parsed too few steps out of release.yml"; }
# 冒烟：无条件的步骤占多数，解析结果里它们的条件字段必须是空的，正文不能挪进条件字段。
[ "$(awk -F "$us" '$1 != "NEEDS" && $2 == "" && $3 != ""' "$work/steps" | wc -l)" -gt 20 ] ||
  { cp "$work/steps" "$work/out"; bad "parsed no unconditional steps with a body out of release.yml"; }
# shellcheck disable=SC2016 # 按字面匹配 workflow 里的表达式
grep -qx '      kind: ${{ steps.kind.outputs.kind }}' .github/workflows/release.yml ||
  bad "the build job of release.yml does not output kind from its kind step"
# agent_touching TEXT：步骤正文是否处理 agent 镜像组。make 目标逐个以 -n 展开判定，不按目标名猜。
# release.yml 不运行 make ci 与 make script-test：同一提交上的整套门禁由主干 CI 承载，gate job 核对它通过（见下面
# gate 的核对），发布时重跑只是重复。这两个目标也不能在这里展开，理由同上面 shellcheck 的核对（script-test 的递归
# $(MAKE) 在 -n 下也会执行，等于在这里递归运行本测试），所以遇到就直接报错、不展开。
agent_touching() {
  case $1 in *readback-agent* | *image-archive/agent*) return 0 ;; esac
  for target in $(printf '%s\n' "$1" | grep -o 'make \(-s \)\{0,1\}[a-z][a-z0-9-]*' | sed 's/^make \(-s \)\{0,1\}//'); do
    case $target in ci | script-test)
      bad "release.yml runs make $target; the gate job relies on ci.yml for that suite:$1"
      continue
      ;;
    esac
    MAKE -n "$target" VERSION=v1.2.3 > "$work/target-n" 2>&1 || { cp "$work/target-n" "$work/out"; bad "make -n $target (from release.yml) failed"; continue; }
    if grep -q 'MANIFEST=agent\|deploy/Dockerfile\.agent' "$work/target-n"; then return 0; fi
  done
  return 1
}
touching=0
# shellcheck disable=SC2094 # 循环里的 awk 只读这个文件，没有写
while IFS=$us read -r job cond text; do
  [ "$job" != NEEDS ] || continue
  agent_touching "$text" || continue
  touching=$((touching + 1))
  case $job in
    build) want="steps.kind.outputs.kind == 'full'" ;;
    *) want="needs.build.outputs.kind == 'full'" ;;
  esac
  [ "$cond" = "$want" ] || bad "release.yml job $job handles the agent image group without 'if: $want':$text"
  if [ "$job" != build ]; then
    needs=$(awk -F "$us" -v j="$job" '$1 == "NEEDS" && $2 == j { print $3 }' "$work/steps")
    case $needs in
      build | *"[build,"* | *" build,"* | *" build]"*) ;;
      *) bad "release.yml job $job reads needs.build.outputs.kind but does not need build (needs: $needs)" ;;
    esac
  fi
done < "$work/steps"
# 冒烟：判定确实认出了 agent 组的步骤（build 的 make docker，三个 job 的 agent 目标，回读记录的上传与下载）。
[ "$touching" -ge 6 ] || bad "only $touching release.yml steps were recognised as handling the agent image group"
for pair in image=docker-hub-push readback=docker-hub-readback promote=docker-hub-promote; do
  awk -F "$us" -v j="${pair%%=*}" -v t="make ${pair#*=} " '$1 == j && $2 == "" && index($3, t) { found = 1 } END { exit !found }' "$work/steps" ||
    bad "release.yml job ${pair%%=*} does not run make ${pair#*=} unconditionally"
done

# 发布清单的接线。两个发布目标的最后一步写构建记录、不再生成 SHA256SUMS；release-manifest 读同一份构建
# 记录与两个镜像组各自的构建记录（与 docker-<组> 写的是同一个路径），仓库名与镜像组的定义相同；release-readback 把
# releasekind 的判定作为 KIND 交给回读。清单的格式与核对由 scripts/release-manifest-test.sh 与 release-assets-test.sh 执行验证。
for pair in full-n=full hub-only-n=hub-only; do
  grep -qx "RECORD=build/release-record scripts/release-manifest.sh record ${pair#*=}" "$work/${pair%%=*}" ||
    bad "release-${pair#*=} does not end by writing build/release-record for kind ${pair#*=}"
  [ "$(tail -n 1 "$work/${pair%%=*}")" = "RECORD=build/release-record scripts/release-manifest.sh record ${pair#*=}" ] ||
    bad "release-${pair#*=} does something after writing its build record"
  ! grep -q 'sha256sum' "$work/${pair%%=*}" ||
    bad "release-${pair#*=} still writes a checksum listing; SHA256SUMS belongs to release-manifest"
done
MAKE -n release-manifest VERSION=v1.2.3 > "$work/out" 2>&1 || bad "make -n release-manifest failed"
call=$(grep 'scripts/release-manifest.sh write$' "$work/out") || bad "release-manifest does not call scripts/release-manifest.sh write"
for want in HUB_REPO=ghcr.io/xjetry/heron-hub AGENT_REPO=ghcr.io/xjetry/heron-agent RECORD=build/release-record DIST=dist \
  HUB_BUILT=build/image-archive/hub/built AGENT_BUILT=build/image-archive/agent/built; do
  [ "$(call_var "$call" "${want%%=*}")" = "${want#*=}" ] || bad "release-manifest passes ${want%%=*}=$(call_var "$call" "${want%%=*}"), want ${want#*=}"
done
# 镜像构建记录的第三列是 check 收到的 AGENT_VERSION，release-manifest 拿它对发布目标的绑定版本；它要说的是镜像里的
# hub 绑定了什么，所以 check 的调用行与镜像里 hub 二进制的 -X main.agentVersion 必须取同一个导出变量 $AGENT_VERSION，
# 调用行上不能另赋别的值（call_var 取最后一次赋值）。docker-agent 的 -n 输出含前置的 docker-hub，两条 check 都核对。
for g in $image_groups; do
  MAKE -n "docker-$g" VERSION=v1.2.3 > "$work/target-n" 2>&1 || bad "make -n docker-$g failed"
  grep -q " BUILT=build/image-archive/$g/built " "$work/target-n" || bad "docker-$g does not write the build record release-manifest reads"
  grep 'scripts/docker-image.sh check$' "$work/target-n" > "$work/check-calls" || bad "make -n docker-$g does not call scripts/docker-image.sh check"
  while IFS= read -r call; do
    # shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文，$AGENT_VERSION 按字面出现
    [ "$(call_var "$call" AGENT_VERSION)" = '"$AGENT_VERSION"' ] ||
      bad "docker-$g passes AGENT_VERSION=$(call_var "$call" AGENT_VERSION) to scripts/docker-image.sh check, want the exported \"\$AGENT_VERSION\" the hub binary binds"
  done < "$work/check-calls"
done
# shellcheck disable=SC2016 # 按字面匹配 make -n 打印的 $AGENT_VERSION
awk -v want='-X main.agentVersion=$AGENT_VERSION"' '{
  n = split($0, parts, /go build /)
  for (i = 2; i <= n; i++) {
    cmd = parts[i]; sub(/[;&|].*/, "", cmd); sub(/ +$/, "", cmd)
    if (substr(cmd, length(cmd) - 9) != " ./cmd/hub") continue
    builds++
    if (!index(cmd, want)) bad++
  }
} END { exit !(builds > 0 && bad == 0) }' "$work/docker-n" ||
  bad "make -n docker builds the hub image binary without -X main.agentVersion=\$AGENT_VERSION, so the build record's AGENT_VERSION column is not what the image binds"
MAKE -n release-readback VERSION=v1.2.3 > "$work/out" 2>&1 || bad "make -n release-readback failed"
call=$(grep 'scripts/release-manifest.sh readback$' "$work/out") || bad "release-readback does not call scripts/release-manifest.sh readback"
# shellcheck disable=SC2016 # 比对的是 make -n 打印的配方原文
case $call in
  'kind=$(go run ./scripts/releasekind -version "$VERSION" -agent "$AGENT_VERSION") && KIND="$kind" '*) ;;
  *) bad "release-readback does not hand the releasekind verdict to the readback: $call" ;;
esac
for want in HUB_REPO=ghcr.io/xjetry/heron-hub AGENT_REPO=ghcr.io/xjetry/heron-agent DIR=build/release-readback; do
  [ "$(call_var "$call" "${want%%=*}")" = "${want#*=}" ] || bad "release-readback passes ${want%%=*}=$(call_var "$call" "${want%%=*}"), want ${want#*=}"
done

# release.yml 的 build job 按 发布目标 → 端到端 → 镜像 → release-manifest → 上传 dist 的顺序：IMAGES 取镜像构建记录，
# SHA256SUMS 要在它之后生成；上传在 release-manifest 之后，签名 job 拿到的才是完整的清单。publish 在建 Release 之后
# 用固定版 buildx 跑 make release-readback（回读全部在那里），不在 workflow 里另取清单、另用 --ignore-missing。
# step_index JOB TEXT：JOB 里正文含 TEXT 的第一个步骤的序号（按文件顺序）；没有时为空。
step_index() {
  awk -F "$us" -v j="$1" -v t="$2" '$1 == j { n++; if (index($3, t)) { print n; exit } }' "$work/steps"
}
# 发布凭主干 CI 放行：有一个 job 无条件运行 scripts/release-ci-gate.sh，核对的是这次 tag 的仓库与提交、带上 job
# token，job 只有 contents 与 actions 的读权限；其余每个 job 都沿 needs（传递地）排在它之后，门禁没过就什么都不
# 构建、不签名、不推送。build 里的 make lint 是发布侧仅剩的静态门禁，要读到之前写进 GITHUB_ENV 的 BUF_AGAINST_REF，
# 所以排在它之后、发布目标之前，且无条件运行。
# shellcheck disable=SC2016 # 按字面匹配 workflow 里的命令与表达式
gate_job=$(awk -F "$us" '$1 != "NEEDS" && $2 == "" && index($3, "run: scripts/release-ci-gate.sh \"$GITHUB_REPOSITORY\" \"$GITHUB_SHA\"") { print $1; exit }' "$work/steps")
if [ -z "$gate_job" ]; then
  bad "release.yml has no unconditional step running scripts/release-ci-gate.sh \"\$GITHUB_REPOSITORY\" \"\$GITHUB_SHA\""
else
  # shellcheck disable=SC2016
  awk -F "$us" -v j="$gate_job" '$1 == j && index($3, "release-ci-gate.sh") && index($3, "GH_TOKEN: ${{ github.token }}") { found = 1 } END { exit !found }' "$work/steps" ||
    bad "release.yml job $gate_job runs the gate without GH_TOKEN: \${{ github.token }}"
  perms=$(awk -v j="  $gate_job:" '
    $0 == j { injob = 1; next }
    injob && /^  [^ ]/ { exit }
    injob && /^    permissions:/ { inperm = 1; next }
    injob && inperm && /^      [a-z-]+: / { sub(/^ +/, ""); print; next }
    injob && inperm { inperm = 0 }
  ' .github/workflows/release.yml | sort | tr '\n' ' ')
  [ "$perms" = "actions: read contents: read " ] ||
    bad "release.yml job $gate_job must hold exactly contents: read and actions: read, has: $perms"
  # needs 的值是单个 job 名或 [a, b]；从 gate 出发反复把"needs 里有已到达的 job"的 job 加进来，剩下的就是不在它之后的。
  ungated=$(awk -F "$us" -v g="$gate_job" '
    $1 == "NEEDS" { n = $3; gsub(/[][ ]/, "", n); needs[$2] = n; next }
    { jobs[$1] = 1 }
    END {
      reach[g] = 1
      do {
        changed = 0
        for (j in needs) {
          if (j in reach) continue
          k = split(needs[j], a, ",")
          for (i = 1; i <= k; i++) if (a[i] in reach) { reach[j] = 1; changed = 1; break }
        }
      } while (changed)
      for (j in jobs) if (!(j in reach)) print j
    }
  ' "$work/steps" | sort | tr '\n' ' ')
  # 空结果不会是解析落空：needs 一行都没解析出时除 gate 外的 job 全部列出；job 本身解析落空由上面的步骤数冒烟挡住。
  [ -z "$ungated" ] || bad "release.yml jobs not ordered after $gate_job through needs: $ungated"
fi
awk -F "$us" '$1 == "build" && $2 == "" && $3 == " run: make lint" { found = 1 } END { exit !found }' "$work/steps" ||
  bad "release.yml build job does not run make lint unconditionally"
prev=0
prev_text=
# shellcheck disable=SC2016 # 按字面匹配 workflow 里的命令
for text in 'echo "BUF_AGAINST_REF=$base" >> "$GITHUB_ENV"' 'run: make lint' 'make "release-$KIND"' 'make e2e' 'make bound-agent-e2e' 'make docker-hub ' 'make docker ' 'make release-manifest ' 'name: dist'; do
  i=$(step_index build "$text")
  case $i in '' | *[!0-9]*) bad "release.yml build job has no step with '$text'"; continue ;; esac
  [ "$i" -gt "$prev" ] || bad "release.yml build job runs '$text' before '$prev_text'"
  prev=$i
  prev_text=$text
done
awk -F "$us" '$1 == "build" && $2 == "" && index($3, "make release-manifest VERSION=\"$GITHUB_REF_NAME\"") { found = 1 } END { exit !found }' "$work/steps" ||
  bad "release.yml build job does not run make release-manifest VERSION=\"\$GITHUB_REF_NAME\" unconditionally"
buildx=$(step_index publish 'scripts/docker-buildx-install.sh')
created=$(step_index publish 'gh release create')
# shellcheck disable=SC2016 # 按字面匹配 workflow 里的命令
readback=$(step_index publish 'make release-readback VERSION="$GITHUB_REF_NAME"')
case "$buildx:$created:$readback" in
  *::* | :* | *:) bad "release.yml publish job lacks the buildx install, gh release create or make release-readback step ($buildx:$created:$readback)" ;;
  *)
    [ "$buildx" -lt "$readback" ] && [ "$created" -lt "$readback" ] ||
      bad "release.yml publish job reads back before installing buildx or creating the release"
    ;;
esac
awk -F "$us" '$1 == "publish" && $2 == "" && index($3, "make release-readback ") { found = 1 } END { exit !found }' "$work/steps" ||
  bad "release.yml publish job does not run make release-readback unconditionally"
if awk -F "$us" '$1 == "publish" && index($3, "gh release download")' "$work/steps" | grep -q .; then
  bad "release.yml publish job downloads assets outside make release-readback"
fi
! grep -q -- '--ignore-missing' .github/workflows/release.yml || bad "release.yml checks a checksum listing with --ignore-missing"

if [ "$failures" != 0 ]; then
  echo "release rules: $failures failure(s)" >&2
  exit 1
fi
echo "release rules ok"
