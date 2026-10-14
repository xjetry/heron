# agent 组：架构表、构建参数、打包配方与打包输入。只发 hub 的门禁（scripts/agentinputs）把这个
# 文件本身与 AGENT_BUNDLE_FILES 当作 agent 组的打包输入，把 agent-go-targets 列出的（包、平台）展开成 Go 源码
# 输入。配方只经这里的变量引用 deploy/ 下的文件：写死在配方里的路径门禁看不见，scripts/release-rules-test.sh
# 核对这个文件里出现的每个 deploy/ 路径都登记在 AGENT_BUNDLE_FILES 里。

# 全部 Go 产物以 CGO_ENABLED=0 构建。export 是全局的，hub 与其余目标照样生效；它决定 agent 组的产物，
# 属于门禁的输入，所以放在这里——主 Makefile 不 export 影响构建的变量，发布规则测试核对这一点。
# 唯一的例外是 Makefile 的 test 目标：-race 在 Linux 上依赖 cgo，它在自己的命令行上以 CGO_ENABLED=1 覆盖这个
# export，只影响那一次 go test，不影响任何产物。
export CGO_ENABLED=0

# 发布产物矩阵：agent 与更新器五个 Linux 架构，agent 两个 darwin 架构（hub 的两个在 Makefile 的 HUB_LINUX_ARCHES）。
# 架构集合只在这几个变量维护，静态门禁、打包清单与门禁的平台清单都由它们展开，不存在第二份清单。
AGENT_LINUX_ARCHES := amd64 arm64 armv7 386 riscv64
AGENT_DARWIN_ARCHES := amd64 arm64
# agent_goarch 把 shell 变量 $$arch（AGENT_LINUX_ARCHES 的一项）映射成 $$goarch、$$goarm 与环境变量串 $$gflags。
# 架构名与 GOARCH 不同名的只有 armv7；打包、build 的编译检查与 agent-go-targets 共用这一处映射。
agent_goarch = case $$arch in armv7) goarch=arm goarm=7 ;; *) goarch=$$arch goarm= ;; esac; gflags="GOARCH=$$goarch$${goarm:+ GOARM=$$goarm}"

# 发布产物的构建参数：版本经 ldflags 注入，-trimpath 去掉构建机路径，-s -w 去掉符号表与 DWARF 调试段。
# 去掉的两样都不在加载段里，程序运行时不读；panic 栈的函数名与文件行号来自 pclntab，发版回读的 go version -m
# 读的是 buildinfo，二者都在加载段里，不受影响。失去的是调试器（delve）读的 DWARF 与 go tool nm 可见的符号表。
# hub 的 HUB_GOFLAGS 也由 RELEASE_LDFLAGS 组成（Makefile），任何一种产物的版本注入与去符号都不会单独漂移。
RELEASE_LDFLAGS = -s -w -X main.version=$(VERSION)
RELEASE_GOFLAGS = -trimpath -ldflags "$(RELEASE_LDFLAGS)"

AGENT_SYSTEMD_UNIT := deploy/systemd/heron-agent.service
AGENT_OPENRC_SCRIPT := deploy/openrc/heron-agent
AGENT_LAUNCHD_PLIST := deploy/launchd/xyz.heron.agent.plist
UPDATER_UNITS := deploy/systemd/heron-updater-agent.service deploy/systemd/heron-updater-hub.service
AGENT_INSTALLERS := deploy/install.sh deploy/install-macos.sh
# agent 镜像（ghcr.io/xjetry/heron-agent）属于 agent 组，只随完整 release 构建与发布。决定它字节的
# 文件登记在这里：镜像组的共享机制与 agent 组参数（deploy/image.mk，含 BuildKit 与基础镜像的 pin、平台集合）、
# Dockerfile、构建上下文的放行清单（.dockerignore）、决定构建器驱动与选项的 scripts/docker-builder.sh。镜像里的
# 二进制是 ./cmd/agent 的 Linux 构建，已由 agent-go-targets 覆盖。只核对或搬运结果的脚本不登记，逐个的理由见
# deploy/image.mk 的 IMAGE_UNBUNDLED。hub 镜像专属的输入（根目录 Dockerfile、hub 镜像名、hub 的冒烟与 checkimage
# 清单）不登记：改它们只影响 hub 镜像，只发 hub 照样可以。
AGENT_IMAGE_FILES := deploy/image.mk deploy/Dockerfile.agent .dockerignore scripts/docker-builder.sh
AGENT_BUNDLE_FILES := deploy/agent.mk $(AGENT_SYSTEMD_UNIT) $(AGENT_OPENRC_SCRIPT) $(AGENT_LAUNCHD_PLIST) $(UPDATER_UNITS) $(AGENT_INSTALLERS) $(AGENT_IMAGE_FILES)

# agent 组的二进制（完整 release）：Linux agent 五个架构、darwin agent 两个架构。darwin 的 CGO_ENABLED=0 由
# 这里的显式 env 承载（静态门禁只收 Linux 产物的原因见 Makefile 的发布目标）。
agent_build = for arch in $(AGENT_LINUX_ARCHES); do \
	  $(agent_goarch); \
	  env GOOS=linux CGO_ENABLED=0 $$gflags go build $(RELEASE_GOFLAGS) -o "dist/build/heron-agent-linux-$$arch" ./cmd/agent; \
	done; \
	for arch in $(AGENT_DARWIN_ARCHES); do \
	  env GOOS=darwin GOARCH=$$arch CGO_ENABLED=0 go build $(RELEASE_GOFLAGS) -o "dist/build/heron-agent-darwin-$$arch" ./cmd/agent; \
	done
AGENT_STATIC = $(addprefix dist/build/heron-agent-linux-,$(AGENT_LINUX_ARCHES))

# 打包的 tar 前设 COPYFILE_DISABLE=1：macOS 的 bsdtar 否则会把扩展属性打成 ._* 条目，busybox 解包会带出多余文件。
# 另加 --no-xattrs：bsdtar 仍会把 com.apple.provenance 之类的扩展属性写成 pax 扩展头，GNU tar 解包时逐条目告警，产物里也带上宿主元数据；
# bsdtar 与 GNU tar 都认这个选项，本地与 CI 构建同一写法。hub 包（Makefile 的 hub_pack）同一写法。
# 说明写在 recipe 之外：recipe 是反斜杠续行拼成的一条 shell 命令，行内的 # 会把其后的续行一并注释掉。
agent_pack = for arch in $(AGENT_LINUX_ARCHES); do \
	  pkg="dist/pkg-$$arch"; mkdir -p "$$pkg"; \
	  cp "dist/build/heron-agent-linux-$$arch" "$$pkg/heron-agent"; \
	  cp $(AGENT_SYSTEMD_UNIT) "$$pkg/heron-agent.service"; \
	  cp $(AGENT_OPENRC_SCRIPT) "$$pkg/heron-agent.openrc"; \
	  COPYFILE_DISABLE=1 tar --no-xattrs -C "$$pkg" -czf "dist/heron-agent_linux_$$arch.tar.gz" heron-agent heron-agent.service heron-agent.openrc; \
	  rm -rf "$$pkg"; \
	done; \
	for arch in $(AGENT_DARWIN_ARCHES); do \
	  pkg="dist/pkg-darwin-$$arch"; mkdir -p "$$pkg"; \
	  cp "dist/build/heron-agent-darwin-$$arch" "$$pkg/heron-agent"; \
	  cp $(AGENT_LAUNCHD_PLIST) "$$pkg/xyz.heron.agent.plist"; \
	  COPYFILE_DISABLE=1 tar --no-xattrs -C "$$pkg" -czf "dist/heron-agent_darwin_$$arch.tar.gz" heron-agent xyz.heron.agent.plist; \
	  rm -rf "$$pkg"; \
	done

# 更新器：$(1) 是架构名列表，AGENT_LINUX_ARCHES 或它的子集 HUB_LINUX_ARCHES。完整 release 为全部架构构建与打包
# （agent 组）；只发 hub 的 release 只为 hub 架构打包，供 install-hub.sh 装 hub 主机的更新器。两路同一份配方，
# 更新器的源码与服务文件因而都是 agent 组的输入——只发 hub 时它们不得有变化，hub 主机与节点上的更新器行为一致。
updater_build = for arch in $(1); do \
	  $(agent_goarch); \
	  env GOOS=linux CGO_ENABLED=0 $$gflags go build $(RELEASE_GOFLAGS) -o "dist/build/heron-updater-linux-$$arch" ./cmd/updater; \
	done
updater_static = $(addprefix dist/build/heron-updater-linux-,$(1))
updater_pack = for arch in $(1); do \
	  pkg="dist/pkg-updater-$$arch"; mkdir -p "$$pkg"; \
	  cp "dist/build/heron-updater-linux-$$arch" "$$pkg/heron-updater"; \
	  cp $(UPDATER_UNITS) "$$pkg/"; \
	  COPYFILE_DISABLE=1 tar --no-xattrs -C "$$pkg" -czf "dist/heron-updater_linux_$$arch.tar.gz" heron-updater $(notdir $(UPDATER_UNITS)); \
	  rm -rf "$$pkg"; \
	done

# 门禁读的两份清单（scripts/agentinputs）。agent-go-targets 每行：包路径 GOOS GOARCH [GOARM]。scripts/stampinstall
# 在构建机上把哈希写进 agent 的安装脚本，它的源码同样决定 agent 组的产物，按 CI 的构建机平台列入。
.PHONY: agent-bundle-inputs agent-go-targets
agent-bundle-inputs:
	@printf '%s\n' $(AGENT_BUNDLE_FILES)
agent-go-targets:
	@for arch in $(AGENT_LINUX_ARCHES); do \
	  $(agent_goarch); \
	  echo "./cmd/agent linux $$goarch $$goarm"; \
	  echo "./cmd/updater linux $$goarch $$goarm"; \
	done; \
	for arch in $(AGENT_DARWIN_ARCHES); do echo "./cmd/agent darwin $$arch"; done; \
	echo "./scripts/stampinstall linux amd64"
