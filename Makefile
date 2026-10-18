# 版本号（VERSION）的两层守卫。不变式：make 展开后的 $(VERSION) 与调用方给的原文逐字相同，且只含
# [A-Za-z0-9_.-]、首字符不是 . 或 -、至多 128 字节。发布物的版本号与镜像 tag 取展开后的 $(VERSION)，
# 而 GitHub Release 建在 release.yml 的原始 tag 名上：展开与原文不同时，二进制、镜像 tag 与 Release 就指向
# 不同的版本号，这正是守卫存在的理由。
#
# make 层：原文里的 $ 会被当作变量或函数引用展开，$(shell …) 在每次引用处执行。make 在执行配方第一行
# 之前就展开整条配方，配方里的 shell 检查拦不住同一配方后面的引用，所以在解析 Makefile 时按未展开的
# 原文（$(value VERSION)）拒绝。这之后 $(VERSION) 的展开是恒等的。
ifneq ($(findstring $$,$(value VERSION)),)
$(error VERSION '$(value VERSION)' contains '$$', which make would expand as a variable or function reference; use a plain version such as v0.1.0)
endif

# hub 绑定的 agent 版本。默认取仓库根 AGENT_VERSION 文件，它是绑定的唯一事实源；命令行可另给，本地
# 验收构建给出与 VERSION 相同的值以产出完整的一套。它经 ldflags 进入 hub 的构建命令，与 VERSION 同理在解析时
# 按原文拒绝 $：命令行给出的值是递归展开变量，其中的 $(shell …) 会在每次引用处执行。用 := 读文件：环境里的
# 同名变量不顶替仓库文件（makefile 里的赋值优先于环境），只有命令行显式给出才覆盖。导出到配方环境：配方一律经
# 环境变量 $$AGENT_VERSION 引用它，不把 $(AGENT_VERSION) 展开拼进 shell 源码，值里的引号或分号因而改写不了命令。读文件只用 shell 内建（read、printf）：
# 解析 Makefile 时就会执行这一行，而发布规则测试在只放了少数只读工具的 PATH 下运行 make（scripts/release-rules-test.sh）。
# 仓库文件提供默认绑定值，只有命令行显式覆盖；hub 构建的 ldflags 与发布种类判定都读导出的同一值。
# 只发 hub 的入口先由 agentinputs 核对绑定版本以来的 agent 组输入，未通过时不进入构建。
AGENT_VERSION := $(strip $(shell read -r v < AGENT_VERSION; printf '%s' "$$v"))
ifneq ($(findstring $$,$(value AGENT_VERSION)),)
$(error AGENT_VERSION '$(value AGENT_VERSION)' contains '$$', which make would expand as a variable or function reference; use a plain version such as v0.1.0)
endif
export AGENT_VERSION

# 下载哪一版已发布 agent。与上面的绑定版本是两个输入：未设置时不在这里报错（make -n 与不下载 agent 的目标都要能解析），
# pinned-agent-e2e 的配方在真正运行时要求它非空。命令行值里的 $ 在解析时拒绝，因为配方把 $(PINNED_AGENT_VERSION)
# 展开进 pin 的 -version，$(shell …) 会在那时执行。
ifneq ($(findstring $$,$(value PINNED_AGENT_VERSION)),)
$(error PINNED_AGENT_VERSION '$(value PINNED_AGENT_VERSION)' contains '$$', which make would expand as a variable or function reference; use a plain version such as v0.15.0)
endif

# shell 层（check_version）：每个消费 VERSION 的目标第一行调用它，只此一份。镜像 tag 与版本号逐字相同
# （heron-hub version 打印的就是 tag），Docker 的 tag 只允许 [A-Za-z0-9_.-]、首字符不为 . 与 -、至多 128 个
# 字符；带构建元数据（+）的 tag 因而不能成为镜像 tag，这类 tag 的发布整体失败，release 的 tar 包
# 与镜像用同一规则，不单独放宽。
# 检查读配方环境里的 $$VERSION，不把 $(VERSION) 拼进 shell 源码：git tag 名允许 ' ( 等字符，拼进去的值
# 能改写检查本身（v1'x' 在单引号里拼接后成了 v1x）。make 把命令行与环境给出的 VERSION 放进配方环境：
# 命令行来源放的是展开值，环境来源放的是原文（GNU make 3.81 与 4.x 实测），二者与原文相同都由上面的
# make 层守卫承载；若日后改为在 Makefile 里赋值而不导出，这里读到空值、报 VERSION is required，不会放行。
# 按字节判断而不用 grep：grep 按行匹配，任一行合法即整体放行，带换行的值会漏过。删掉允许的字节后只应
# 剩下末尾的哨兵 /（删完后剩下的换行都落在末尾，没有哨兵就会被命令替换吞掉）；shell 的 ${VERSION##[.-]*}
# 为空即首字符是 . 或 -。通过之后 $(VERSION) 只含 [A-Za-z0-9_.-]，配方里其余位置直接展开它才是安全的。
check_version = if [ -z "$$VERSION" ]; then echo "VERSION is required, e.g. VERSION=v0.1.0" >&2; exit 1; fi; \
	if [ "$$(printf '%s/' "$$VERSION" | LC_ALL=C tr -d 'A-Za-z0-9_.-')" != / ] || [ -z "$${VERSION\#\#[.-]*}" ] || [ $${\#VERSION} -gt 128 ]; then \
	  echo "VERSION '$$VERSION' cannot be an image tag: only [A-Za-z0-9_.-], not starting with . or -, at most 128 characters, no + build metadata" >&2; exit 1; fi

.PHONY: gen lint lint-sh shellcheck-path test build hub-binary binaries ci e2e e2e-matrix pinned-agent-e2e bound-agent-e2e fixtures web-install web-lint web-test web-e2e web release-full release-hub-only release-manifest release-readback release-kind release-goflags hub-goflags release-breaking-base agent-version script-test docker release-channel netstructure-live

web-install:
	pnpm --dir web install --frozen-lockfile

# TS 客户端与 Go 代码同一口径：都由 buf 生成并入库，ci 要求生成目录没有改动或未跟踪文件。
gen: web-install
	buf generate

# buf breaking 的对照 ref。lint 判定的是"这棵树相对它的基线没有破坏兼容的 proto 改动"，基线随检出的形状不同：
# 本地默认本仓库的 main（任务分支在工作树里跑 lint 就能发现；在 main 自身上跑是与自己比较，恒通过）；
# ci.yml 的 pull_request 检出没有本地 main，传 origin/main；release.yml 的检出是 detached 的 tag，没有任何本地分支，
# 传上一个发布 tag（release-breaking-base）——发出去的 proto 要与已经发出去的兼容；仓库的第一个发布没有上一个 tag，
# 传 none，只在 HEAD 之前确实没有发布时有效（scripts/buf-breaking.sh 复核）。
# 用 ref= 而不是 branch=：branch= 只认本地分支名，tag 与 remote-tracking ref 都只能经 ref= 给出。
BUF_AGAINST_REF ?= main
lint: lint-sh
	go mod tidy -diff
	buf lint
	scripts/buf-breaking.sh "$(BUF_AGAINST_REF)"
	@unformatted="$$(gofmt -l $$(git ls-files '*.go'))"; if [ -n "$$unformatted" ]; then printf 'gofmt: %s\n' $$unformatted >&2; exit 1; fi
	go vet ./...
	GOOS=linux go vet ./...
	GOOS=darwin go vet ./...
	go tool staticcheck ./...
	@# 另外两个 GOOS 用已经按本机平台编译好的二进制：`GOOS=linux go tool staticcheck` 会把工具本身交叉编译成 linux 产物再执行，exec 直接失败；
	@# go tool -n 给出本机产物的路径（缺失时先构建），对它设 GOOS 才是"按另一个平台分析这份源码"。
	staticcheck=$$(go tool -n staticcheck) && GOOS=linux "$$staticcheck" ./... && GOOS=darwin "$$staticcheck" ./...
	go tool govulncheck ./...
	go run ./scripts/releasekind -check-file AGENT_VERSION

# shellcheck 固定为一个版本的官方发布包（koalaman/shellcheck 的 GitHub release）。不同版本的诊断不同（同一段
# `cond && a || b`，0.9.0 报 SC2015、0.11.0 不报），lint 的结论要只取决于源码，本机、ci.yml 与 release.yml 就必须
# 执行同一个二进制：Makefile 里每一次 shellcheck 调用都写成 $(SHELLCHECK)，它只由下面的文件目标安装，不看 PATH
# 上有没有、是哪一版 shellcheck。scripts/release-rules-test.sh 对 .PHONY 列出的目标（script-test 与 ci 除外，理由
# 在该测试里）逐个 make -n，展开后的命令里以 shellcheck 为名的词必须恰好是 $(SHELLCHECK)，否则指出那一行失败；
# 两个发布目标的实际执行另由 scripts/release-assets-test.sh 钉住（PATH 上放了一旦被调用就失败的 shellcheck）。
# SHELLCHECK_SHA256 每个受支持的宿主平台一条，<发布包平台名>=<SHA-256>；不在清单里的平台由安装脚本报错。SHA-256 取自
# GitHub release 资产的 digest：gh api repos/koalaman/shellcheck/releases/tags/v0.11.0 --jq '.assets[] | .name + " " + .digest'。
# 安装路径带版本号：文件目标没有前置条件，make 只凭文件存在就不再安装，所以换版本必须换路径，改 SHELLCHECK_VERSION
# 即落到一个还不存在的文件上、重新下载校验，已装的旧版本不会被沿用。下载、校验与原子落盘在 scripts/shellcheck-install.sh：
# 放在脚本里而不是配方里，它自己也由 lint-sh 用这个 shellcheck 检查。
SHELLCHECK_VERSION := v0.11.0
SHELLCHECK_SHA256 := \
	darwin.aarch64=339b930feb1ea764467013cc1f72d09cd6b869ebf1013296ba9055ab2ffbd26f \
	darwin.x86_64=c2c15e08df0e8fbc374c335b230a7ee958c313fa5714817a59aa59f1aa594f51 \
	linux.aarch64=68a8133197a50beb8803f8d42f9908d1af1c5540d4bb05fdfca8c1fa47decefc \
	linux.x86_64=b7af85e41cc99489dcc21d66c6d5f3685138f06d34651e6d34b42ec6d54fe6f6
SHELLCHECK := build/tools/shellcheck-$(SHELLCHECK_VERSION)/shellcheck

$(SHELLCHECK):
	scripts/shellcheck-install.sh $(SHELLCHECK_VERSION) $@ $(SHELLCHECK_SHA256)

# 网卡结构规则的真实内核实测：privileged 容器、内核 7.0.14-orbstack，容器架构须与本机一致。需要 Docker。
netstructure-live:
	scripts/netstructure-live.sh

shellcheck-path:
	@echo $(SHELLCHECK)

lint-sh: $(SHELLCHECK)
	$(SHELLCHECK) -s sh deploy/install.sh deploy/install-hub.sh deploy/install-macos.sh deploy/openrc/heron-agent scripts/docker-smoke.sh scripts/docker-agent-smoke.sh scripts/docker-agent-smoke-test.sh scripts/docker-image.sh scripts/docker-image-test.sh scripts/release-rules-test.sh scripts/release-assets-test.sh scripts/release-manifest.sh scripts/release-manifest-test.sh scripts/image-platform-ref.sh scripts/docker-builder.sh scripts/docker-builder-test.sh scripts/docker-buildx-install.sh scripts/netstructure-live.sh scripts/netstructure-live-setup.sh
	$(SHELLCHECK) -s sh scripts/pinned-agent-download.sh scripts/pinned-agent-e2e.sh scripts/pinned-agent-download-test.sh scripts/pinned-agent-e2e-args-test.sh
	$(SHELLCHECK) -s sh scripts/release-breaking-base.sh scripts/buf-breaking.sh scripts/release-breaking-base-test.sh scripts/release-ci-gate.sh scripts/release-ci-gate-test.sh
	$(SHELLCHECK) -s sh scripts/update-credentials-accept.sh scripts/shellcheck-install.sh scripts/web-assets-check.sh
	$(SHELLCHECK) -s sh scripts/wirebench/install-agent.sh scripts/wirebench/measure-agent.sh scripts/wirebench/provenance.sh scripts/wirebench/rss-darwin.sh scripts/wirebench/rss.sh scripts/wirebench/run.sh scripts/wirebench/sizes.sh

# deploy 包的替身测试每个用例都用 sh 真跑一遍安装脚本，脚本再逐个 exec 替身与装出来的假二进制，整包要跑成百上千个
# 进程。macOS 对新写出的可执行文件首次执行有一段等待（同一文件再执行不等，CPU 空闲时同样存在，Linux 没有），整包墙钟
# 的大头在这里，开发机上可以超过 go test 给每个测试二进制的默认 10 分钟超时，到点被杀看起来像随机失败。替身已按内容在
# 测试进程内只写一份（deploy/main_test.go），装出来的假二进制仍按用例新建。只给这个包放宽到 20 分钟，其余包保持默认，
# 卡住的测试仍尽早暴露。包清单先落到变量：go list 失败或清单为空时整条命令失败，它的退出码不会被管道
# 吞掉；再滤掉 deploy，滤空时 grep 以 1 退出、同样失败。两处都防的是退化成不带包参数、只测当前目录。
# -race：hub 的内存缓存、写协程与跨包锁序都靠并发纪律维持，竞态只有检测器照得到，没有它的绿灯证明不了这些不变式。
# 检测器下各包慢 3–10 倍，store 包在本机实测约 10 分钟，贴着默认的每包超时，所以放宽到 20 分钟。
# deploy 包是替身脚本测试，没有并发可查，不带 -race。
# deploy/agent.mk 对所有配方 export CGO_ENABLED=0（产物一律纯 Go 构建），而 -race 在 Linux 上依赖 cgo（Go 1.27.2
# 的 cmd/go 只对 darwin 豁免，所以 macOS 开发机上看不到这个失败）：go test -race 会以 "requires cgo" 退出。这条命令
# 因此在自己的命令行上显式 CGO_ENABLED=1，只覆盖这一次 go test 的环境；产物构建仍由 agent.mk 的 export 决定。
test:
	all=$$(go list ./...) && [ -n "$$all" ] && pkgs=$$(printf '%s\n' "$$all" | grep -vx github.com/xjetry/heron/deploy) && CGO_ENABLED=1 go test -race -count=1 -timeout 20m $$pkgs
	go test -count=1 -timeout 20m ./deploy/

# 发布规则（版本号守卫、预发布判定）与回读判定的回归检查：只跑 make 的检查、-n 展开与 docker 桩，
# 不构建、不访问 registry。
script-test:
	MAKE='$(MAKE)' scripts/release-rules-test.sh
	scripts/release-assets-test.sh
	scripts/release-manifest-test.sh
	scripts/docker-image-test.sh
	scripts/docker-builder-test.sh
	scripts/docker-agent-smoke-test.sh
	scripts/pinned-agent-download-test.sh
	scripts/pinned-agent-e2e-args-test.sh
	scripts/release-breaking-base-test.sh
	scripts/release-ci-gate-test.sh

# 类型检查覆盖全部 tsconfig（面板与公开页、vite 配置、e2e、测试），lint 按各文件所属的 tsconfig 取类型信息。
# react-hooks 的推荐集把几条规则定为 warn，eslint 遇 warn 仍退出 0；--max-warnings 0 让门禁不带 warning 通过。
web-lint: web-install
	pnpm --dir web exec tsc -b
	pnpm --dir web exec eslint . --max-warnings 0

# vitest 的 include 只收 src 下的 DOM 用例。代理行为测试要在真实 TLS 连接上跨过 Node 默认的空闲超时，
# 用 CI 同一个 Node 的 node:test 跑，不经过 jsdom。
web-test: web-install
	pnpm --dir web exec vitest run
	node --test web/e2e/proxy-behavior.mjs

# 真实浏览器验证正式 hub 的同域主题与 Passkey；运行前执行 pnpm --dir web exec playwright install。
web-e2e: hub-binary
	pnpm --dir web exec playwright test

# 两个入口的产物落在 internal/hub/web/dist（面板）与 dist-public（公开页）供 go:embed；不入库，缺产物时 hub 也能编译并给出说明页。
web: web-install
	pnpm --dir web run build
	scripts/web-assets-check.sh

# build 验证全部已有的包在本机、Linux 与 darwin 的 amd64、arm64 上都能编译。darwin 的采集文件带 build tag，
# Linux 上的 CI 里 lint 的 GOOS=darwin go vet 只为本机架构编译它们；另一个架构只有这里编译得到，而 purego 按架构分文件实现。
# agent 与更新器还要按 release 的 AGENT_LINUX_ARCHES 逐架构编译：其中 armv7、386 是 32 位，int/uint 只有 32 位，
# 常量转换与溢出只在这些架构上暴露；只编 64 位的 ci 会放过它们，等到发布构建才失败。
# 二进制产物由 binaries 生成，只有 e2e 需要它。
build:
	go build ./...
	GOOS=linux GOARCH=amd64 go build ./...
	GOOS=linux GOARCH=arm64 go build ./...
	GOOS=darwin GOARCH=amd64 go build ./...
	GOOS=darwin GOARCH=arm64 go build ./...
	@set -e; for arch in $(AGENT_LINUX_ARCHES); do \
	  $(agent_goarch); \
	  echo "GOOS=linux $$gflags go build ./cmd/agent ./cmd/updater"; \
	  env GOOS=linux CGO_ENABLED=0 $$gflags go build ./cmd/agent ./cmd/updater; \
	done

# 开发构建的 hub 同样绑定仓库文件里的 agent 版本，面板与节点在线更新按发布时的口径工作；版本仍是 dev。
hub-binary: web
	go build -ldflags "-X main.agentVersion=$$AGENT_VERSION" -o bin/heron-hub ./cmd/hub

binaries: hub-binary
	GOOS=linux GOARCH=amd64 go build -o bin/heron-agent-linux-amd64 ./cmd/agent
	GOOS=linux GOARCH=arm64 go build -o bin/heron-agent-linux-arm64 ./cmd/agent
	go run ./scripts/checkstatic bin/heron-agent-linux-amd64 bin/heron-agent-linux-arm64

# ci 汇总生成物、静态检查、竞态测试、脚本与前端门禁；e2e 需要独占 Docker 资源，须单独运行。
ci: gen lint test script-test web-lint web-test web build
	status="$$(git status --porcelain -- gen web/src/gen)" || exit $$?; \
	if [ -n "$$status" ]; then printf '%s\n' "$$status"; exit 1; fi

fixtures:
	scripts/capture-proc.sh docker-debian

# 一级发行版每次都跑；二级发版前跑。镜像与期望系统名成对，前一个失败就停。
E2E_TIER1 := debian:bookworm-slim=Debian alpine:3.21=Alpine
E2E_TIER2 := ubuntu:24.04=Ubuntu rockylinux:9=Rocky

e2e: binaries
	@for pair in $(E2E_TIER1); do \
	  AGENT_IMAGE="$${pair%%=*}" EXPECT_OS="$${pair#*=}" scripts/e2e.sh || exit $$?; \
	done

e2e-matrix: binaries
	@for pair in $(E2E_TIER1) $(E2E_TIER2); do \
	  AGENT_IMAGE="$${pair%%=*}" EXPECT_OS="$${pair#*=}" scripts/e2e.sh || exit $$?; \
	done

# 下载哪一版已发布 agent 由 PINNED_AGENT_VERSION 决定：boundagent pin 按它写出清单，AGENT_PIN 把清单交给端到端。
# hub 仍由 hub-binary 用导出的 AGENT_VERSION 注入，不读 PINNED_AGENT_VERSION。清单在被忽略的 build/ 下，不进入仓库。
pinned-agent-e2e: hub-binary
	@if [ -z "$(PINNED_AGENT_VERSION)" ]; then echo "PINNED_AGENT_VERSION is required, e.g. PINNED_AGENT_VERSION=v0.15.0" >&2; exit 1; fi
	@mkdir -p build/bound-agent
	go run ./scripts/boundagent pin -version "$(PINNED_AGENT_VERSION)" -out build/bound-agent/pin.json -linux-arches "$(AGENT_LINUX_ARCHES)" -darwin-arches "$(AGENT_DARWIN_ARCHES)"
	AGENT_PIN=build/bound-agent/pin.json scripts/pinned-agent-e2e.sh $(E2E_TIER1)

# 绑定版本的端到端是钉住版本等于 AGENT_VERSION 的特例。目标专属变量只在这个目标及其前置上生效，
# 直接跑 pinned-agent-e2e 时不会被它盖住。
bound-agent-e2e: PINNED_AGENT_VERSION = $(AGENT_VERSION)
bound-agent-e2e: pinned-agent-e2e

# agent 组的架构表、构建参数、打包配方与打包输入登记集中在 deploy/agent.mk：只发 hub 的门禁把这个
# 片段本身当作 agent 组的输入，主 Makefile 只保留 hub 自己的架构与构建参数。
include deploy/agent.mk

HUB_LINUX_ARCHES := amd64 arm64

# hub 的版本注入：版本与绑定的 agent 版本都经 ldflags 注入，-trimpath 去掉构建机路径；
# 版本与去符号部分与 agent 组共用 deploy/agent.mk 的 RELEASE_LDFLAGS，任何一种产物的这两项都不会单独漂移。
# agent 与更新器没有 agentVersion 这个变量，不给它们。
HUB_GOFLAGS = -trimpath -ldflags "$(RELEASE_LDFLAGS) -X main.agentVersion=$$AGENT_VERSION"

# 不经发布目标、却要构建与发布产物同形的 agent 与 hub 的脚本（scripts/wirebench/run.sh）从这两个目标取构建参数，
# 不另抄一份：抄下的参数在这里改动时不会跟着变，测出来的就不再是发布产物。输出是 RELEASE_GOFLAGS 与 HUB_GOFLAGS
# 展开后的文字，即发布配方交给 shell 的那段参数，调用方以 shell 词法 eval 成参数列表，与配方的解释方式相同。
# 单引号原样打印：check_version 通过后 VERSION 只含 [A-Za-z0-9_.-]，变量里只有双引号；hub 那段里的
# $AGENT_VERSION 留给调用方的 shell 展开，与配方里取自环境同理。
release-goflags:
	@$(check_version)
	@printf '%s\n' '$(RELEASE_GOFLAGS)'
hub-goflags:
	@$(check_version)
	@printf '%s\n' '$(HUB_GOFLAGS)'

# 一个 Linux hub 二进制：$(1) 为 GOARCH，$(2) 为输出路径。发布目标与 docker 镜像都调用它。
hub_build = env GOOS=linux GOARCH=$(1) CGO_ENABLED=0 go build $(HUB_GOFLAGS) -o "$(2)" ./cmd/hub

# 发布目标：完整 release 与只发 hub 各一个，调用方先取 make -s release-kind 的判定再调用对应的目标。
# 两个目标开头各自再判定一次，不符即失败，单独调用也绕不过规则。不设在 make 里再分派的 release：make 对含
# $(MAKE) 的配方行在 -n 下也整行执行，判定会在 dry-run 里真的跑（发布规则测试的受限 PATH 把 go 设成绊线）。
# 同理 $(MAKE) web 单独占一行。本地验收与线上发布调用同一组目标，产物与版本注入完全一致。
#
# 静态门禁只收 Linux 产物：它守的是"与 libc 无关"（不带 PT_INTERP 与 DT_NEEDED），这是 Linux 发行版之间的约束。
# darwin 产物是 Mach-O，依赖 /usr/lib/libSystem.B.dylib 与 libresolv.9.dylib（otool -L）：Go 在 darwin 上经 libSystem
# 发起系统调用，macOS 也不提供静态链接的系统库，"不带动态依赖"在这里不成立也无须成立；darwin 的 CGO_ENABLED=0
# 由 agent_build 的显式 env 承载（deploy/agent.mk）。
# VERSION 在构建前端之前检查：不合规时立即报错，不等 web 目标跑完；AGENT_VERSION 的判定紧跟其后，判定不过时
# 不产出任何东西。
# 三个安装脚本在全部 tar 包打好之后写入本版版本号与每个 tar 包的 SHA-256（deploy/releasestamp），脚本只按
# 写进去的清单校验；写入读的是 dist 下的 *.tar.gz，所以必须排在打包之后。两个目标不生成 SHA256SUMS：清单要覆盖
# 镜像 digest（IMAGES），而镜像在发布目标与端到端之后才构建，所以清单由 release-manifest 最后生成（见那里）。两个
# 目标的最后一步写构建记录 RELEASE_RECORD（VERSION、AGENT_VERSION、发布种类），release-manifest 凭它确认清单与
# 这一次的产物同属一组参数。写入后的脚本在这里过 shellcheck：make lint 只查源码，这一步查的是实际发布的字节，
# 写入格式出错时在发布任何东西之前失败。用的是 lint 那个固定版本的 $(SHELLCHECK)，配方里先 $(MAKE) 安装它（已装
# 时什么都不做）：不写成目标的前置条件，前置条件先于配方第一行执行，而版本号守卫必须是第一个动作，在它拒绝之前
# 不能有下载（scripts/release-rules-test.sh 钉住守卫是 -n 输出的第一行、被拒时输出的第一行）。
assert_release_kind = kind=$$(go run ./scripts/releasekind -version "$$VERSION" -agent "$$AGENT_VERSION") || exit 1; \
	[ "$$kind" = $(1) ] || { echo "release-$(1) does not match this release's kind: $$kind" >&2; exit 1; }
# 清掉上一次的产物、清单与构建记录：构建中途失败时不留下一份还指着旧产物的记录。
release_clean = rm -rf dist/build dist/*.tar.gz dist/SHA256SUMS dist/AGENT_VERSION dist/IMAGES dist/install.sh dist/install-hub.sh dist/install-macos.sh $(RELEASE_RECORD) && mkdir -p dist/build
# 构建记录的读写都在 scripts/release-manifest.sh。放在 build/ 而不是 dist/：release.yml 把 dist/* 整体上传为发布资产。
RELEASE_RECORD := build/release-record
release_record = RECORD=$(RELEASE_RECORD) scripts/release-manifest.sh record $(1)
hub_binaries = for arch in $(HUB_LINUX_ARCHES); do $(call hub_build,$$arch,dist/build/heron-hub-linux-$$arch); done
HUB_STATIC = $(addprefix dist/build/heron-hub-linux-,$(HUB_LINUX_ARCHES))
hub_pack = for arch in $(HUB_LINUX_ARCHES); do \
	  pkg="dist/pkg-hub-$$arch"; mkdir -p "$$pkg"; \
	  cp "dist/build/heron-hub-linux-$$arch" "$$pkg/heron-hub"; \
	  cp deploy/systemd/heron-hub.service "$$pkg/heron-hub.service"; \
	  COPYFILE_DISABLE=1 tar --no-xattrs -C "$$pkg" -czf "dist/heron-hub_linux_$$arch.tar.gz" heron-hub heron-hub.service; \
	  rm -rf "$$pkg"; \
	done

release-full:
	@$(check_version)
	@$(call assert_release_kind,full)
	$(MAKE) $(SHELLCHECK)
	$(MAKE) web
	$(release_clean)
	@set -e; $(agent_build); $(call updater_build,$(AGENT_LINUX_ARCHES)); $(hub_binaries)
	go run ./scripts/checkstatic $(AGENT_STATIC) $(call updater_static,$(AGENT_LINUX_ARCHES)) $(HUB_STATIC)
	@set -e; $(agent_pack); $(call updater_pack,$(AGENT_LINUX_ARCHES)); $(hub_pack)
	rm -rf dist/build
	go run ./scripts/stampinstall -version $(VERSION) -dir dist $(AGENT_INSTALLERS) deploy/install-hub.sh
	$(SHELLCHECK) -s sh dist/install.sh dist/install-hub.sh dist/install-macos.sh
	$(call release_record,full)

# 只发 hub：门禁排在任何构建之前，agent 组的输入自绑定版本以来变过就不产出任何东西。hub 组之外，
# agent 的两个安装脚本取自绑定版本的 release，验签与核对都在 scripts/boundagent 里。复制来的两个脚本不是本版构建
# 的，stampinstall 不写它们；它们与 hub 组一起进本版的 SHA256SUMS（release-manifest）。
release-hub-only:
	@$(check_version)
	@$(call assert_release_kind,hub-only)
	go run ./scripts/agentinputs -base "$$AGENT_VERSION"
	$(MAKE) $(SHELLCHECK)
	$(MAKE) web
	$(release_clean)
	@set -e; $(call updater_build,$(HUB_LINUX_ARCHES)); $(hub_binaries)
	go run ./scripts/checkstatic $(call updater_static,$(HUB_LINUX_ARCHES)) $(HUB_STATIC)
	@set -e; $(call updater_pack,$(HUB_LINUX_ARCHES)); $(hub_pack)
	rm -rf dist/build
	go run ./scripts/stampinstall -version $(VERSION) -dir dist deploy/install-hub.sh
	$(SHELLCHECK) -s sh dist/install-hub.sh
	go run ./scripts/boundagent fetch -version "$$AGENT_VERSION" -dir dist -linux-arches "$(AGENT_LINUX_ARCHES)" -darwin-arches "$(AGENT_DARWIN_ARCHES)"
	$(call release_record,hub-only)

# hub 镜像组：ghcr.io/xjetry/heron-hub:<version>。构建、核对、推送、回读与 promote 的机制与 agent 组共用，
# 在 deploy/image.mk；这里只给 hub 专属的参数，它们不是 agent 组的输入，改它们不要求完整发布。镜像里不编译 Go：
# hub 二进制经 hub_build 构建（与发布包同一组参数），面板与公开页随 go:embed 进二进制，所以先 make web——漏掉它，
# 镜像里的 /admin/ 与 / 都只有 503 说明页。冒烟（scripts/docker-smoke.sh）在 make docker-hub 装入本机平台之后、
# 发布后回读按平台 docker pull 之后由 scripts/docker-image.sh 调用；docker-hub-smoke 冒烟本机已装入的那一个。
include deploy/image.mk
IMAGE_hub_REPO := $(IMAGE_REGISTRY)/xjetry/heron-hub
IMAGE_hub_DOCKERFILE := Dockerfile
IMAGE_hub_BIN := heron-hub
IMAGE_hub_BUILD := hub_build
IMAGE_hub_SMOKE := scripts/docker-smoke.sh
IMAGE_hub_PREPARE := web
IMAGE_hub_SMOKE_HUB :=
$(eval $(call image_group,hub))

# 发布清单：在 dist 写 AGENT_VERSION、IMAGES，再生成覆盖全部资产的 SHA256SUMS；格式与核对在
# scripts/release-manifest.sh。顺序固定为发布目标 → 端到端 → 镜像（make docker 或 make docker-hub）→ release-manifest：
# IMAGES 取镜像构建记录里的 digest，SHA256SUMS 要在它之后生成才覆盖得到。只写这三个清单、不构建：构建记录或安装
# 脚本与本次的 VERSION / AGENT_VERSION 对不上就失败并点名，dist 里不留清单。镜像仓库与构建记录的位置取自镜像组的定义。
release_image_repos = HUB_REPO=$(IMAGE_hub_REPO) AGENT_REPO=$(IMAGE_agent_REPO)
release-manifest:
	@$(check_version)
	$(release_image_repos) RECORD=$(RELEASE_RECORD) DIST=dist HUB_BUILT=$(image_hub_built) AGENT_BUILT=$(image_agent_built) scripts/release-manifest.sh write

# 发布后的回读（release.yml 的 publish，在 gh release create 之后）：从 Release 页面取回清单，验签后逐项核对，并按
# IMAGES 匿名读 registry 上版本 tag 当前的索引 digest。发布种类取自 scripts/releasekind，绑定版本取配方环境里的
# AGENT_VERSION，即 make -s agent-version 打印的那一个。取回的文件放在 RELEASE_READBACK_DIR，必须是一个还不存在的目录。
RELEASE_READBACK_DIR := build/release-readback
release-readback:
	@$(check_version)
	kind=$$(go run ./scripts/releasekind -version "$$VERSION" -agent "$$AGENT_VERSION") && KIND="$$kind" $(release_image_repos) DIR=$(RELEASE_READBACK_DIR) scripts/release-manifest.sh readback

# 本地构建并核对两个镜像组：各自构建两个平台、导出布局，核对后把本机平台装进 docker 并冒烟。ci.yml 调用它；
# release.yml 的 build job 在完整 release 时调用它，只发 hub 时只调用 docker-hub。
docker: $(addprefix docker-,$(IMAGE_GROUPS))

# 预发布判定：去掉构建元数据（+ 及之后）后仍含 - 就是预发布。GitHub Release 是否标为 prerelease、
# 镜像是否移动 latest 都读它，判定只在这一处。纯文本函数，不经 shell：make 展开配方时就求值，早于配方
# 里的 check_version，求值本身不能执行任何东西。
RELEASE_CHANNEL = $(if $(findstring -,$(firstword $(subst +, ,$(VERSION)))),prerelease,stable)

release-channel:
	@$(check_version)
	@echo $(RELEASE_CHANNEL)

# 发布种类：full 或 hub-only，判定只在 scripts/releasekind。调用方据此调用 release-full 或
# release-hub-only，两个目标开头也读它。
release-kind:
	@$(check_version)
	@go run ./scripts/releasekind -version "$$VERSION" -agent "$$AGENT_VERSION"

# 发布树做 buf breaking 的基线，release.yml 把它写进 BUF_AGAINST_REF。判定在 scripts/release-breaking-base.sh：VERSION 之前
# 最近的发布 tag，仓库的第一个发布打印 none；不退回 main——release 检出里没有本地分支，退回只是换一种失败。
release-breaking-base:
	@$(check_version)
	@scripts/release-breaking-base.sh "$$VERSION"

# 发布流水线的回读读它（release.yml 里安装脚本回读取绑定版本），不在 workflow 里另读一遍仓库文件。
agent-version:
	@printf '%s\n' "$$AGENT_VERSION"
