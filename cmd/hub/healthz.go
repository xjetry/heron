package main

import "net/http"

// healthzHandler 是 /healthz 的处理器：无鉴权、只回 "ok\n"，不输出任何版本、节点数、配置或计数。
//
// 就绪由服务生命周期保证：newHub 在 store.Open、全部 Load 和服务构造成功后才调用 newHandler 挂载它；
// hub.run 在启动更新门 Gate 成功后才调用 srv.Serve(listener)。所以 net.Listen 之后、Serve 之前的连接只进
// 内核队列、得不到应答；若提前单独服务 health，就会在索引未就绪或候选未确认时误报就绪。
//
// 它是普通 HTTP 路径，不是 PublicService 的挂载点，所以不进公开服务的限流桶。
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}
