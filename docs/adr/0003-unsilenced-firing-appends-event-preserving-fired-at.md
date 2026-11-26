# 维护静默解除补发采用新事件且保留原触发时刻

在维护静默期内进入 `firing` 的节点×规则，若在静默与维护状态全部解除时仍处于 `firing`，引擎不修改静默期内已落库的 `silenced = true` 历史事件，而是在解除后追加写入一条当前时刻的 `firing` 事件（`silenced = false`）并创建投递，同时保持状态行中的 `fired_at` 为最初进入 `firing` 的时刻。

原地回填旧事件的投递会打破 `AlertEvent.silenced == true` 必无投递行的审计契约并篡改历史状态；新增专用 `transition` 枚举则会破坏现有自定义 Webhook 模板对 `firing` / `recovered` 的解析。追加一条标准 `firing` 事件并保留最初 `fired_at`，使审计历史、下游模板兼容性与恢复通知中的故障真实持续时长三者同时成立。
