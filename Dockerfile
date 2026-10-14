# check=skip=InvalidDefaultArgInFrom
# hub 镜像：FROM scratch，只含静态二进制、CA 证书与非 root 用户。
# 镜像里不编译 Go：make docker 用与 release 同一条构建命令把二进制产出到 build/image/linux/<arch>/，
# 这里按 TARGETARCH 取用。根文件系统由 scripts/checkimage 逐条目核对，改这里须同步改那份清单。

# 基础镜像由 make 经 --build-arg 传入（Makefile 的 ALPINE_IMAGE，按 digest 固定，冒烟的工具镜像取同一个）；
# 没有传入时 FROM 为空、构建失败，不会退回浮动的 tag。故意不给默认值，首行的 check 指令关掉的正是
# "ARG 默认值使 FROM 为空"这条告警；它是解析器指令，必须在文件第一行。
ARG ALPINE_IMAGE

# 这一阶段只产出与架构无关的文件，固定在构建节点的本机平台上运行；最终阶段没有 RUN，
# 所以多平台构建不在目标架构上执行任何程序。
FROM --platform=$BUILDPLATFORM ${ALPINE_IMAGE} AS rootfs
# CA 证书供通知出站 HTTPS（Go 在 Linux 上先读 /etc/ssl/certs/ca-certificates.crt）；
# --upgrade 取 3.21 仓库里当前的证书包，而不是基础镜像构建时的那份。
# /data 交给运行用户：空的命名卷或匿名卷挂到 /data 时，Docker 把镜像里这个目录的属主带到卷上。
# /tmp 为 1777：SQLite 的排序、临时表与建索引在页缓存装不下时写临时文件，没有可写的临时目录时报
# disk I/O error (6410)，小查询不触发；root 属主的 1777 目录对任何 uid 可写，不依赖 USER 取值。
RUN apk add --no-cache --upgrade ca-certificates-bundle \
 && mkdir -p /rootfs/etc/ssl/certs /rootfs/data \
 && mkdir -m 1777 /rootfs/tmp \
 && cp /etc/ssl/certs/ca-certificates.crt /rootfs/etc/ssl/certs/ \
 && printf 'heron-hub:x:65532:65532:heron-hub:/nonexistent:/sbin/nologin\n' > /rootfs/etc/passwd \
 && printf 'heron-hub:x:65532:\n' > /rootfs/etc/group \
 && chown 65532:65532 /rootfs/data

FROM scratch
ARG TARGETARCH
LABEL org.opencontainers.image.source="https://github.com/xjetry/heron"
# COPY --from 保留源阶段的属主（/data 为 65532），来自构建上下文的文件归 root。
COPY --from=rootfs /rootfs/ /
# 放在容器的默认 PATH 里：docker exec <容器> heron-hub passwd … 按名字就能执行。
# 权限位由这里的 --chmod 指定，不随构建机的 umask：COPY 否则沿用构建上下文里文件的权限位，而 go build
# 产出的文件是 0777 去掉构建者的 umask（umask 002 的机器上是 0775）。
COPY --chmod=0755 build/image/linux/${TARGETARCH}/heron-hub /usr/local/bin/heron-hub
# 数字形式，与 /data 的属主、/etc/passwd 里的账户是同一个 uid，不经名字解析。
USER 65532:65532
VOLUME /data
EXPOSE 8080
# HEALTHCHECK 调镜像内的 heron-hub health（scratch 里没有 curl，也没有 shell，所以用不经过 shell 的 exec 形式）。
# 探针默认 URL 就是容器默认的监听地址，与 CMD 的 --listen 一致。间隔 30s：反代与编排器要能在半分钟内看到
# 不健康实例；超时 5s 大于探针自身的 3s 总时限，留给进程调度；重试 3 次避免一次抖动就判死；起始宽限 5s 覆盖
# 打开库与加载索引，宽限内不计入重试。探针不跟随重定向，也不进公开限流桶（它不是 PublicService 挂载点）。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 CMD ["/usr/local/bin/heron-hub", "health", "--url", "http://127.0.0.1:8080"]
ENTRYPOINT ["/usr/local/bin/heron-hub"]
# 容器里监听非 loopback 是预期的，hub 的启动告警照旧；反代与 --trusted-proxies 由部署者配。
CMD ["serve", "--db", "/data/heron.db", "--listen", "0.0.0.0:8080"]
