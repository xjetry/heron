# 独立公开监听端口保留主监听全量超集并共享限流

启用 `--public-listen` 时，副监听端口仅挂载无管理入口标记（`admin_path = ""`）的 `PublicService` 与公开页资源，而主监听 `--listen` 完整保留包含公开页与 `PublicService` 在内的全量路由，且两端口共用同一个 `PublicService` 实例。

管理后台的主题短期预览（`PreviewTheme`）依赖主监听同源相对路径，主监听保留公开页可避免引入跨源预览凭据传递；副监听物理不注册 `/admin/`、`AdminService` 与 `AgentService`，使反向代理无需编写脆弱的路径屏蔽规则即可隔离前后台域名，同时共用 `PublicService` 实例保证来源限流配额与快照字节缓存不被双端口翻倍绕过。
