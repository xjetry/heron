# 镜像组：hub 镜像与 agent 镜像共用的构建、核对、推送、回读与 promote 机制，以及 agent 组的
# 定义。每个组由 image_group 宏展开成一组静态目标（docker-<组>、-smoke、-push、-readback、-promote），make -n 的
# 输出是固定的行，scripts/release-rules-test.sh 逐行核对；hub 组的参数在主 Makefile，agent 组的在本文件末尾。
#
# 本文件登记在 AGENT_BUNDLE_FILES 里（deploy/agent.mk）：这里的配方、pin 与 agent 组参数决定 agent 镜像的字节，
# 只发 hub 的门禁（scripts/agentinputs）因而把它的任何改动都判成 agent 组的改动，下次必须完整发布。hub 专属的
# 输入（根目录 Dockerfile、hub 镜像名、checkimage 里 hub 的清单、scripts/docker-smoke.sh）不在本文件，改它们不
# 牵动 agent 组。本文件引用的仓库路径逐个裁决，裁决由 scripts/release-rules-test.sh 核对（新增引用而不裁决即失败）：
# 决定镜像字节的登记进 AGENT_BUNDLE_FILES，只核对或搬运结果的列在 IMAGE_UNBUNDLED，理由见那里。

# registry 主机。两个组的仓库都必须在它下面（image_group 在解析时核对）：release.yml 只登录这一个 registry
# （make docker-registry），推送与登录因而不会指向两个不同的 registry。
IMAGE_REGISTRY := ghcr.io
# 镜像平台：两个组相同，只在这里维护；checkimage 按架构认 ELF 机器类型，加平台要同时改那里。
IMAGE_ARCHES := amd64 arm64
comma := ,
empty :=
space := $(empty) $(empty)
IMAGE_PLATFORMS := $(subst $(space),$(comma),$(addprefix linux/,$(IMAGE_ARCHES)))
# 构建上下文里的二进制按 linux/<arch>/<二进制名> 排列，.dockerignore 只放行这些文件，两个组的 Dockerfile 按
# TARGETARCH 各取各的。不放在 dist/ 下：release.yml 以 dist/* 整体上传发布资产，而镜像在发布目标之后、
# gh release create 之前构建，目录混进 dist/ 就会被 dist/* 展开进上传参数。
IMAGE_BIN_DIR := build/image
# 每个组的 OCI 布局、构建记录与回读记录都在 IMAGE_ARCHIVE_DIR/<组>/ 下，两组互不覆盖。release.yml 的 build job
# 把整个 IMAGE_ARCHIVE_DIR 作为 image artifact 交给推送、回读与 promote；不放在 dist/ 下，理由同 IMAGE_BIN_DIR。
IMAGE_ARCHIVE_DIR := build/image-archive
# 构建节点固定为这一版 BuildKit（docker-container 驱动），本地、CI 与发布用同一个：docker 自带的 docker 驱动在
# 经典镜像存储上不支持多平台构建，而是否启用 containerd 存储是宿主的配置。构建器名带版本号：改 BUILDKIT_VERSION
# 即换用新构建器，旧的不会被沿用；不用时 docker buildx rm 删除。同名构建器由 scripts/docker-builder.sh 核对
# 驱动、镜像与驱动选项后才沿用（只改 digest 时名字不变），不存在时创建。名字沿用它只服务 hub 镜像时的写法，
# 两个组用同一个构建器。
BUILDKIT_VERSION := v0.33.0
BUILDKIT_IMAGE := moby/buildkit:$(BUILDKIT_VERSION)@sha256:6c2fa84a6b61ccd72899dde4239f8d5717f05f9a8ca6f3cad185fb1a95a94de3
DOCKER_BUILDER := heron-hub-buildkit-$(BUILDKIT_VERSION)
# alpine 按 digest 固定，只在这里定义：两个 Dockerfile 的 rootfs 阶段经 --build-arg 取用，冒烟的工具镜像（读卷
# 属主、预置不可写的卷、在 hub 的网络命名空间里发请求）经环境变量 TOOL_IMAGE 取用。
ALPINE_IMAGE := alpine:3.21@sha256:ce64758a109eb420d874a118f87920e625e12d3634e03b4a5573fd9f6e5d3507
ensure_builder = scripts/docker-builder.sh $(DOCKER_BUILDER) $(BUILDKIT_IMAGE)
# checkimage 只依赖标准库与本仓库的包：持有推送权限的 image job 要编译它（scripts/checkimage 的测试守着依赖）。
# GOPROXY=off 不许下载任何模块：image job 不恢复 Go 缓存，checkimage 日后多了第三方依赖时这一步在那里编译失败，
# 不会把第三方代码编译进那个 job；本机与 build job 的模块缓存里已有的模块照常可用。
build_checkimage = mkdir -p build/tools && GOPROXY=off go build -o build/tools/checkimage ./scripts/checkimage

# 不登记进 AGENT_BUNDLE_FILES 的脚本：它们只核对或搬运已经构建好的镜像，改不了发布出去的字节。
#   scripts/docker-image.sh：导出、载入、推送、回读与 promote 都按构建记录里的索引 digest 引用布局或 registry，
#     push 要求布局里的索引等于记录、verify 要求 registry 上的等于记录、promote 要求两份记录相等，改动它只能让
#     这些比对失败，不能让另一个索引通过。
#   scripts/image-platform-ref.sh：回读时把索引 digest 解析成平台清单 digest，拉取的内容由 digest 决定。
#   scripts/checkimage、scripts/checkstatic：只读产物并判定，不写任何东西。
#   scripts/docker-agent-smoke.sh：只运行已核对的镜像并判定。
# 判定这些脚本的正确性靠它们各自的测试（scripts/docker-image-test.sh 等），不靠完整发布。
IMAGE_UNBUNDLED := scripts/docker-image.sh scripts/image-platform-ref.sh scripts/checkimage scripts/checkstatic scripts/docker-agent-smoke.sh

# 已展开的组，docker 目标按它汇总。
IMAGE_GROUPS :=

# image_group 组名：展开一个镜像组的全部目标。调用前给出：
#   IMAGE_<组>_REPO         镜像仓库，必须在 IMAGE_REGISTRY 下
#   IMAGE_<组>_DOCKERFILE   Dockerfile 路径（构建上下文都是仓库根）
#   IMAGE_<组>_BIN          二进制在 IMAGE_BIN_DIR/linux/<arch>/ 下的文件名，与 Dockerfile 的 COPY 来源、.dockerignore 的放行项一致
#   IMAGE_<组>_BUILD        构建一个 Linux 二进制的函数名，$(1) 为 GOARCH、$(2) 为输出路径；必须与发布产物同一组参数
#   IMAGE_<组>_SMOKE        冒烟脚本
#   IMAGE_<组>_PREPARE      可空：构建二进制之前要先跑的 make 目标（hub 的面板随 go:embed 进二进制，要先 make web）
#   IMAGE_<组>_SMOKE_HUB    可空：冒烟要对着注册的 hub 镜像所属的组。给出时 docker-<组> 以 docker-<该组> 为前置条件，
#                           scripts/docker-image.sh 按那个组的构建记录从它的布局载入 hub，交给冒烟的是载入后的镜像 ID
# checkimage 的清单名就是组名（scripts/checkimage 里每组一份清单）。
#
# docker-<组>：构建两个平台、导出 OCI 布局，核对后把本机平台装进 docker 并冒烟（scripts/docker-image.sh check），
# 通过后记下 <版本> <索引 digest> <AGENT_VERSION>。第三列与 hub 二进制（HUB_GOFLAGS 的 -X main.agentVersion）取同一个
# 导出变量 AGENT_VERSION（主 Makefile 的 export），写在 check 的调用行上让 make -n 看得见这一处取值，
# scripts/release-rules-test.sh 据此核对两处取的是同一个变量；release-manifest 再核对它等于发布目标的 AGENT_VERSION
# （分步调用漏给 AGENT_VERSION 时镜像里的 hub 与 tar 包里的 hub 绑定不同的版本）。第一行是版本守卫：它之前不能有任何
# 动作（PREPARE 的递归 make 排在它之后）。
# docker-<组>-smoke：冒烟本机 docker 里已有的 <仓库>:<VERSION>（docker-<组> 装入的那一个）。SMOKE_PLATFORM（为空时用
# docker 的默认平台）由 make 从命令行或环境放进配方环境，冒烟脚本直接读，不在这里重新赋值拼进 shell 源码。
# docker-<组>-push / -readback / -promote：release.yml 的 image、readback、promote 三个 job 各调用一个，都只取
# build job 交出的 IMAGE_ARCHIVE_DIR（readback 另交出回读记录给 promote），之后 publish 才 gh release create。三个
# 目标只调用 docker、本仓库的脚本与只依赖标准库的 checkimage，不构建镜像、不碰前端与第三方 Go 模块；持推送权限
# 的是 push 与 promote，它们不起任何容器，运行被构建镜像的只有不持写权限的 readback（scripts/release-rules-test.sh
# 逐行钉住三个目标的配方，scripts/docker-image-test.sh 钉住 push 与 promote 只调用 imagetools）。
# 不变式：latest 只会指向回读通过的镜像；回读通过的就是 make docker-<组> 核对过的那个索引（scripts/docker-image.sh）。
#   push：按构建记录的索引 digest 从布局推送版本 tag，不碰 latest。
#   readback：匿名回读 registry 上版本 tag 指向的索引（IMAGE_BIN_DIR 下没有构建产物、digest 等于构建记录、匿名可取、
#     逐平台冒烟、逐平台核对根文件系统），通过后记下回读记录。
#   promote：两份记录必须是同一个 digest、版本 tag 仍指向它。正式版本把 latest 移到这个 digest 并回读确认；预发布
#     确认 latest 没有指向它。
# 宏里的 $$ 经 call 展开一次后才交给 eval 解析，所以 shell 的 $arch 写作 $$$$arch，配方执行时才展开的 make 引用写作 $$(…)。
define image_group
$(if $(filter $(IMAGE_REGISTRY)/%,$(IMAGE_$(1)_REPO)),,$(error IMAGE_$(1)_REPO '$(IMAGE_$(1)_REPO)' is not under IMAGE_REGISTRY '$(IMAGE_REGISTRY)'; release.yml logs in to that registry only))
IMAGE_GROUPS += $(1)
.PHONY: docker-$(1) docker-$(1)-smoke docker-$(1)-push docker-$(1)-readback docker-$(1)-promote
# 本组的 OCI 布局、构建记录与回读记录。
image_$(1)_layout := $(IMAGE_ARCHIVE_DIR)/$(1)/layout
image_$(1)_built := $(IMAGE_ARCHIVE_DIR)/$(1)/built
image_$(1)_record := $(IMAGE_ARCHIVE_DIR)/$(1)/readback
# scripts/docker-image.sh 的子命令读同一组参数，镜像名、架构、构建器、布局位置与清单名只在这里给出。
image_$(1)_env := IMAGE_REPO=$(IMAGE_$(1)_REPO) ARCHES='$(IMAGE_ARCHES)' BUILDER=$(DOCKER_BUILDER) TOOL_IMAGE='$(ALPINE_IMAGE)' \
  SMOKE=$(IMAGE_$(1)_SMOKE) CHECKIMAGE=build/tools/checkimage LAYOUT='$$(abspath $$(image_$(1)_layout))' BUILT=$$(image_$(1)_built) MANIFEST=$(1)
# 运行冒烟的子命令（check、smoke、verify）另带冒烟要对着注册的 hub 的布局与构建记录（那个组的，不是本组的）；
# push 与 promote 不起容器，不带。递归展开：那个组可能在本组之后才展开，它的路径变量到配方执行时才有值。
# 非空时以一个空格开头，配方里直接接在 image_<组>_env 后面。
image_$(1)_smoke_env = $(if $(IMAGE_$(1)_SMOKE_HUB),$$(space)HUB_LAYOUT='$$(abspath $$(image_$(IMAGE_$(1)_SMOKE_HUB)_layout))' HUB_BUILT=$$(image_$(IMAGE_$(1)_SMOKE_HUB)_built))

docker-$(1): $(if $(IMAGE_$(1)_SMOKE_HUB),docker-$(IMAGE_$(1)_SMOKE_HUB))
	@$$(check_version)
$(if $(IMAGE_$(1)_PREPARE),	$$(MAKE) $(IMAGE_$(1)_PREPARE))
	rm -rf $(foreach a,$(IMAGE_ARCHES),$(IMAGE_BIN_DIR)/linux/$(a)/$(IMAGE_$(1)_BIN)) $(IMAGE_ARCHIVE_DIR)/$(1)
	@set -e; for arch in $(IMAGE_ARCHES); do \
	  mkdir -p "$(IMAGE_BIN_DIR)/linux/$$$$arch"; \
	  $$(call $(IMAGE_$(1)_BUILD),$$$$arch,$(IMAGE_BIN_DIR)/linux/$$$$arch/$(IMAGE_$(1)_BIN)); \
	done
	go run ./scripts/checkstatic $(foreach a,$(IMAGE_ARCHES),$(IMAGE_BIN_DIR)/linux/$(a)/$(IMAGE_$(1)_BIN))
	$$(ensure_builder)
	docker buildx build --builder $(DOCKER_BUILDER) -f $(IMAGE_$(1)_DOCKERFILE) --build-arg ALPINE_IMAGE=$(ALPINE_IMAGE) --platform $(IMAGE_PLATFORMS) --output type=oci,dest=$$(image_$(1)_layout),tar=false,name=$(IMAGE_$(1)_REPO):$$(VERSION) .
	$$(build_checkimage)
	$$(image_$(1)_env)$$(image_$(1)_smoke_env) AGENT_VERSION="$$$$AGENT_VERSION" scripts/docker-image.sh check

docker-$(1)-smoke:
	@$$(check_version)
	$$(image_$(1)_env)$$(image_$(1)_smoke_env) scripts/docker-image.sh smoke

docker-$(1)-push:
	@$$(check_version)
	$$(image_$(1)_env) scripts/docker-image.sh push

docker-$(1)-readback:
	@$$(check_version)
	$$(ensure_builder)
	$$(build_checkimage)
	$$(image_$(1)_env)$$(image_$(1)_smoke_env) IMAGE_BIN_DIR=$(IMAGE_BIN_DIR) RECORD=$$(image_$(1)_record) scripts/docker-image.sh verify

docker-$(1)-promote:
	@$$(check_version)
	$$(image_$(1)_env) CHANNEL=$$(RELEASE_CHANNEL) RECORD=$$(image_$(1)_record) scripts/docker-image.sh promote
endef

.PHONY: docker-registry
docker-registry:
	@echo $(IMAGE_REGISTRY)

# agent 组：ghcr.io/xjetry/heron-agent，只随完整 release 构建、推送、回读与 promote（release.yml 按
# 发布种类调用它的目标）。二进制与 tar 包里的 Linux agent 同一组构建参数（deploy/agent.mk 的 RELEASE_GOFLAGS）。
# 冒烟要对着本次构建的 hub 注册并上报，所以 hub 组是它的前置。
IMAGE_agent_REPO := $(IMAGE_REGISTRY)/xjetry/heron-agent
IMAGE_agent_DOCKERFILE := deploy/Dockerfile.agent
IMAGE_agent_BIN := heron-agent
image_agent_build = env GOOS=linux GOARCH=$(1) CGO_ENABLED=0 go build $(RELEASE_GOFLAGS) -o "$(2)" ./cmd/agent
IMAGE_agent_BUILD := image_agent_build
IMAGE_agent_SMOKE := scripts/docker-agent-smoke.sh
IMAGE_agent_PREPARE :=
IMAGE_agent_SMOKE_HUB := hub
$(eval $(call image_group,agent))
