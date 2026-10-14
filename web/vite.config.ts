import react from "@vitejs/plugin-react";
import { readFileSync, writeFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { defineConfig, type Plugin } from "vitest/config";

import { asyncUtilTimeout } from "./src/test/async-timeout.ts";

// 两个入口各自打包，产物直接落在 internal/hub/web 的 embed 目录，不再拷贝一次：
// 默认模式是管理面板（web/index.html，base /admin/，落到 dist）；--mode public 是公开页
// （根为 src/public，base /，落到 dist-public）。base 必须与 hub 的挂载路径一致，产物里的资源引用才能命中。
// tsconfig.app.json 关掉了 erasableSyntaxOnly：protoc-gen-es 为 proto enum 生成 TS enum，那条限制会拒绝生成代码。
const embedDir = (name: string) => fileURLToPath(new URL(`../internal/hub/web/${name}`, import.meta.url));

// Vite 清空输出目录，也由构建自身恢复占位文件，保证任意构建入口都维持 embed 目录含文件（go:embed 的模式要能匹配）。
// 不落盘的构建（build.write 为 false，importScan.test 读模块图用）既不清空也不产出，不碰 embed 目录。
function keepEmbedDirectory(outDir: string): Plugin {
  let write = true;
  return {
    name: "keep-embed-directory",
    apply: "build",
    configResolved(config) { write = config.build.write; },
    closeBundle() { if (write) writeFileSync(`${outDir}/.gitkeep`, ""); },
  };
}

// 内嵌字体（src/fonts.ts）是 SIL OFL 授权，再分发须随字体附上许可证全文；woff2 的 name 表只有版权行与许可证网址，
// 没有全文。所以把字体包自带的 LICENSE 原样产出到每份产物的 licenses/ 下，随 woff2 一起嵌进 hub。
// 这张表与 src/fonts.ts 引入的字体包一一对应，fonts.test.ts 双向核对；增减字体包两处一起改。
const FONT_PACKAGES = ["@fontsource-variable/inter", "@fontsource-variable/jetbrains-mono"];
function fontLicenses(): Plugin {
  return {
    name: "font-licenses",
    apply: "build",
    generateBundle() {
      for (const pkg of FONT_PACKAGES) {
        const source = readFileSync(new URL(`./node_modules/${pkg}/LICENSE`, import.meta.url));
        this.emitFile({ type: "asset", fileName: `licenses/${pkg.split("/")[1]}-OFL.txt`, source });
      }
    },
  };
}

// 内置配色只写在 styles.css 的 --accent: light-dark(浅色, 深色) 里。外观页的取色器在主色为空时显示浅色那一个值，
// 这里在构建与测试时读出它、经 define 编成常量（lib/palette.ts），不另存一份；写法变了读不出即构建失败。
// 不在运行时用 ?raw 读 styles.css：vitest 默认把 CSS 文件换成空内容，测试里读不到。
function builtInLightAccent(): string {
  const css = readFileSync(new URL("./src/styles.css", import.meta.url), "utf8");
  const m = /--accent:\s*light-dark\(\s*(#[0-9a-fA-F]{6})\s*,/.exec(css);
  if (!m) throw new Error("src/styles.css: --accent must be light-dark(#rrggbb, …); the appearance page reads the built-in light accent from it");
  return m[1].toLowerCase();
}

export default defineConfig(({ mode }) => {
  const isPublic = mode === "public";
  const outDir = embedDir(isPublic ? "dist-public" : "dist");
  return {
    root: isPublic ? fileURLToPath(new URL("./src/public", import.meta.url)) : undefined,
    base: isPublic ? "/" : "/admin/",
    plugins: [react(), keepEmbedDirectory(outDir), fontLicenses()],
    define: { __BUILT_IN_ACCENT__: JSON.stringify(builtInLightAccent()) },
    // 字体必须以文件形式同源加载：hub 的 CSP 是 default-src 'self'（internal/hub/web/web.go），没有放行 data:，
    // 而 Vite 默认把小于 4 KiB 的资源内联成 data: URL——被内联的字体子集会被浏览器按 CSP 拒绝加载（JetBrains Mono
    // 的 cyrillic-ext 子集约 2 KiB 就会撞上）。产物由 scripts/web-assets-check.sh 在构建后核对。其余资源沿用默认阈值。
    build: { outDir, emptyOutDir: true, assetsInlineLimit: (filePath) => (/\.(woff2?|ttf|otf)$/.test(filePath) ? false : undefined) },
    test: {
      include: ["src/**/*.test.{ts,tsx}"],
      environment: "jsdom",
      setupFiles: ["./src/test/setup.ts"],
      globals: false,
      // 必须长于异步查找上界，否则运行器会先杀掉用例，上界到不了。
      testTimeout: asyncUtilTimeout * 2,
    },
  };
});
