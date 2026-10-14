import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: false,
  workers: 1,
  timeout: 45_000,
  use: { baseURL: "https://localhost:18988", ignoreHTTPSErrors: true },
  webServer: {
    command: "node e2e/server.mjs",
    url: "https://localhost:18988/admin/",
    ignoreHTTPSErrors: true,
    reuseExistingServer: false,
    timeout: 30_000,
    // 不配时 Playwright 收尾直接 SIGKILL 进程组，e2e/server.mjs 的退出处理不执行，临时目录（库、证书）留在 $TMPDIR。
    // SIGTERM 让 hub 排空并关库后退出，server.mjs 随之退出并删目录。hub 排空在途请求至多 drainTimeout（10 秒，
    // cmd/hub/serve.go），收尾时测试已结束、没有在途请求；30 秒是超出它之后仍不退出才强杀的上限。
    gracefulShutdown: { signal: "SIGTERM", timeout: 30_000 },
  },
  projects: [
    { name: "chromium", use: { browserName: "chromium" } },
    { name: "firefox", use: { browserName: "firefox" } },
    { name: "webkit", use: { browserName: "webkit" } },
  ],
});
