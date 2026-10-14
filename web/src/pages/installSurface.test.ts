// @vitest-environment node
import { readFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { describe, expect, it } from "vitest";
import { REPO_URL } from "../lib/repo";

const root = resolve(dirname(fileURLToPath(import.meta.url)), "../../..");
const installSh = readFileSync(resolve(root, "deploy/install.sh"), "utf8");
// 面板的安装命令由 lib/installCommand.ts 拼出、在 components/InstallCommands.tsx 里展示与说明：两处出现的选项都算面板的。
const panel = ["web/src/components/InstallCommands.tsx", "web/src/lib/installCommand.ts"].map((file) => readFileSync(resolve(root, file), "utf8")).join("\n");
const userscript = readFileSync(resolve(root, "web/src/assets/heron-quick-node.user.js"), "utf8");

describe("面板安装命令与 install.sh", () => {
  it("命令里的每个 --xxx 都是 install.sh 接受的选项", () => {
    const accepted = new Set([...installSh.matchAll(/^\s*(--[A-Za-z0-9-]+)\)/gm)].map((m) => m[1]));
    const used = [...new Set([...panel.matchAll(/--[A-Za-z0-9-]+/g)].map((m) => m[0]))];
    expect(used).toEqual(expect.arrayContaining(["--hub", "--key", "--insecure-http", "--re-register", "--update-source", "--base-url"]));
    for (const flag of used) {
      expect(accepted, `${flag} 不在 install.sh 的 case 标签里`).toContain(flag);
    }
  });

  // 油猴脚本另拼一份安装命令（它是独立分发的脚本，不能引用面板代码），同样只能用 install.sh 接受的选项。
  // 前后不贴连字符或字母数字，避开注释里的分隔线。
  it("油猴脚本命令里的每个 --xxx 也都是 install.sh 接受的选项", () => {
    const accepted = new Set([...installSh.matchAll(/^\s*(--[A-Za-z0-9-]+)\)/gm)].map((m) => m[1]));
    const used = [...new Set([...userscript.matchAll(/(?<![\w-])--[a-z][a-z0-9-]*/g)].map((m) => m[0]))];
    expect(used).toEqual(expect.arrayContaining(["--hub", "--key", "--insecure-http", "--update-source"]));
    for (const flag of used) {
      expect(accepted, `${flag} 不在 install.sh 的 case 标签里`).toContain(flag);
    }
  });

  // 脚本只装自己所属的版本：版本由面板取哪个 URL 的脚本决定，命令里没有第二个版本来源。
  it("install.sh 不接受 --version，面板命令也不带", () => {
    const accepted = new Set([...installSh.matchAll(/^\s*(--[A-Za-z0-9-]+)\)/gm)].map((m) => m[1]));
    expect(accepted).toContain("--insecure-http");
    expect(accepted).not.toContain("--version");
    expect(panel).not.toMatch(/--version\b/);
  });

  // 面板的仓库地址只有 lib/repo.ts 一个来源：安装命令里出现 github 字面地址就是第二份来源，release 地址必须由 REPO_URL 派生。
  it("install.sh 的 REPO 与面板的仓库地址一致；安装命令不另写仓库地址", () => {
    const repo = installSh.match(/^REPO=(\S+)/m)?.[1];
    expect(repo).toBeTruthy();
    expect(REPO_URL).toBe(repo);
    expect(panel).not.toMatch(/https:\/\/github\.com\//);
    expect(panel).toMatch(/\$\{REPO_URL\}\/releases\//);
  });

  // 油猴脚本独立分发、不能引用面板代码，只能自带一份仓库地址；每一处都必须落在 install.sh 的 REPO 之下。
  it("油猴脚本里的仓库地址与 install.sh 的 REPO 一致", () => {
    const repo = installSh.match(/^REPO=(\S+)/m)?.[1];
    const urls = [...userscript.matchAll(/https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+/g)].map((m) => m[0]);
    expect(urls.length).toBeGreaterThan(0);
    for (const url of urls) expect(url === repo || url.startsWith(`${repo}/`), url).toBe(true);
  });
});
