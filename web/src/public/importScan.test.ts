// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, relative, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { build } from "vite";
import { beforeAll, expect, it } from "vitest";

const web = resolve(dirname(fileURLToPath(import.meta.url)), "../..");

// bundle 按真实的构建配置打一遍、不落盘，返回模块图里的全部模块 id 与产物。入口的发现（index.html 里的每个
// module script）与 import 的文法（静态、动态、import.meta.glob……）都由打包器自己决定，测试不另写一份。
async function bundle(mode: string) {
  const out = await build({ configFile: resolve(web, "vite.config.ts"), mode, logLevel: "silent", build: { write: false } });
  const output = (Array.isArray(out) ? out : [out]).flatMap((o) => {
    if (!("output" in o)) throw new Error(`vite build --mode ${mode} returned a watcher, not a bundle`);
    return o.output;
  });
  const ids = output.flatMap((o) => (o.type === "chunk" ? o.moduleIds : []));
  return { output, ids };
}

// 管理服务的生成代码：protoc-gen-es 只生成 _pb.ts（服务描述符也在里面），connect-es 的 _connect.ts 若以后出现也算。
const isAdminGen = (id: string): boolean => /\/gen\/heron\/v1\/admin_(pb|connect)\.ts$/.test(id);
// 第二道核对看产物的字节：admin.proto 的文件描述符（fileDesc 的 base64 实参）开头一段只会出现在带着它的包里。
function descriptorPrefix(file: string): string {
  const m = /fileDesc\("([A-Za-z0-9+/]{40})/.exec(readFileSync(resolve(web, "src/gen/heron/v1", file), "utf8"));
  if (!m) throw new Error(`${file}: no fileDesc("…") call`);
  return m[1];
}

let pub: Awaited<ReturnType<typeof bundle>>, panel: Awaited<ReturnType<typeof bundle>>;
beforeAll(async () => {
  [pub, panel] = [await bundle("public"), await bundle("production")];
}, 60_000);

it("公开包的模块图不含管理服务的生成代码", () => {
  // 冒烟：确实打进了公开服务的生成代码与共用组件，空图不能冒充通过。
  for (const f of ["gen/heron/v1/public_pb.ts", "gen/heron/v1/query_pb.ts", "components/History.tsx", "components/Chart.tsx", "lib/billing.ts"]) {
    expect(pub.ids.some((id) => id.endsWith(`/src/${f}`)), f).toBe(true);
  }
  expect(pub.ids.filter(isAdminGen).map((id) => relative(web, id))).toEqual([]);
});

it("公开包的产物里没有 admin.proto 的描述符", () => {
  const code = pub.output.filter((o) => o.type === "chunk").map((o) => o.code).join("\n");
  expect(code.includes(descriptorPrefix("public_pb.ts")), "public.proto descriptor in the public bundle").toBe(true);
  expect(code.includes(descriptorPrefix("admin_pb.ts")), "admin.proto descriptor in the public bundle").toBe(false);
});

// site.ts 的"自定义 CSS 排在全部内置样式之后"依赖这一条：公开包没有按需加载的 chunk，CSS 全由 index.html 的 <link> 引入。
it("公开包的 CSS 都在 index.html 里静态引入，没有按需加载的 chunk", () => {
  const index = pub.output.find((o) => o.fileName === "index.html");
  if (index?.type !== "asset") throw new Error("public bundle has no index.html asset");
  const html = typeof index.source === "string" ? index.source : new TextDecoder().decode(index.source);
  const css = pub.output.filter((o) => o.fileName.endsWith(".css"));
  expect(css.length).toBeGreaterThan(0);
  for (const o of css) expect(html).toContain(`<link rel="stylesheet" crossorigin href="/${o.fileName}">`);
  for (const o of pub.output.filter((o) => o.type === "chunk")) expect(o.dynamicImports, o.fileName).toEqual([]);
});

it("同一套检查从面板包看得到管理服务", () => {
  expect(panel.ids.some(isAdminGen)).toBe(true);
  const code = panel.output.filter((o) => o.type === "chunk").map((o) => o.code).join("\n");
  expect(code.includes(descriptorPrefix("admin_pb.ts")), "admin.proto descriptor in the panel bundle").toBe(true);
});
