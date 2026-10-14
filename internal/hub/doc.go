// Package hub 只承载跨包的说明，没有代码。
//
// # 跨包锁序
//
// 各子包在自己的注释里说明自己的锁保护什么、包内的取锁顺序；会在持有一个包的锁时去取另一个包的锁的顺序集中写在
// 这里，子包注释指向这里。箭头表示"持有左边时可以去取右边"，反方向一律不允许；新增一条跨包持锁调用，先在这里登记
// 并核对不成环。
//
//	nodeops.Service.mu（节点编辑锁）
//	  → alert.Engine.writeMu（UpdateScope）→ probe.Registry.writeMu（UpdateScope 的 mutate 里的 UpdateNode、
//	    BatchUpdateNodeTags）→ probe.Registry.mu
//	  → auth.Auth.mutMu（DeleteNode）
//	  → traffic.Book.writeMu → traffic.Book.mu（Commit）；traffic.Book.mu（SetResetDay）
//	  → ingest.Service.mu（SetAddressPins）
//	auth.Auth.mutMu → probe.Registry.writeMu → probe.Registry.mu（建节点经 auth.NodeCreator 落库并发布）
//	alert.Engine.writeMu → traffic.Book.mu（流量评估读 Committed）；→ alert.Queue（flush 时 Enqueue，不阻塞）
//	ingest.Service.pendingMu → ingest.Service.stateMu（Forget 取写侧，Report 的鉴权与内存写入取读侧）
//	  → auth.Auth.mu、probe.Registry.mu、updates.Manager 的锁、live、traffic.Book.mu、限流桶、ingest.Service.mu
//	ingest.Service.mu → auth.Auth.mu（facts 写入的完成回调里复查 token）
//
// 不成环的依据：alert、auth、probe、traffic、ingest 都不 import nodeops，它们经构造或 setter 注入的实现（auth 的
// NodeCreator 是 probe.Registry，告警引擎的 Sender 是 alert.Queue、流量源是 traffic.Book）也都不在 nodeops，任何持
// 这些锁的路径都取不到 nodeops.Service.mu；auth、probe、updates、live、traffic 都不 import ingest 与 alert，持它们的
// 锁取不到 ingest 与告警引擎的锁；probe 只 import store，持 probe 的锁取不到任何上表里的其它锁。只看 import 方向不够：
// 注入的实现若来自上游包，也能在下游的锁之下取到上游的锁，改注入时要重新核对这一段。
//
// 下面这些调用刻意不在任何上表的锁之下做，因为它们要等别人放锁或等在途工作退出：
//   - nodeops.Service.Forget（及它调用的 ingest.Service.Forget、alert.Engine.Forget）：等在途上报、写协程回调与
//     评估退出；nodeops 在放掉 mu 之后才调用。库外删除的清理（nodeops.Reloader）在 Forget 之前只取一次 mu 当屏障，
//     随即放掉。
//   - ingest.Service.Forget 先在 ingest 的锁之外调 probe.Registry.Forget（取 writeMu）与 updates.Manager.Forget，
//     再按 pendingMu → stateMu → mu 取 ingest 自己的锁。
//
// store 处在链的末端：除下面的例外，持上表任何锁时都可以同步等 store 的写事务。例外来自写协程里执行的异步写完成
// 回调：facts 写入的回调取 ingest.Service.mu、再取 auth.Auth.mu 的读侧，所以持这两把锁时不得同步等待 store 的写，
// 否则写协程与等待者互等（auth.Auth.mu 的临界区本就不含 I/O）。给异步写加取锁的回调，要把那把锁加进这条例外。
//
// # 进程内缓存与库外写入
//
// token 映射（auth）与探测任务缓存（probe）由库派生，启动时建立；离线子命令是库外写者，每个提交的写事务推进库里的
// 离线变更代数（store.ExternalWriter），运行中的 hub 按周期读代数并重载这两份缓存、清理库外删除的节点
// （nodeops.Reloader）。各缓存按各自的锁独立发布，重载是最终一致的，不跨缓存原子。
package hub
