import js from "@eslint/js";
import { defineConfig } from "eslint/config";
import reactHooks from "eslint-plugin-react-hooks";
import globals from "globals";
import tseslint from "typescript-eslint";

// 类型感知的规则经 projectService 按每个文件所属的 tsconfig（tsconfig.json 的 references）取类型信息：
// 出货代码归 tsconfig.app.json，测试归 tsconfig.test.json，e2e 归 tsconfig.e2e.json，vite 配置归 tsconfig.node.json。
// 不在任何 tsconfig 里的文件没有类型信息，只能整文件关掉类型感知规则（disableTypeChecked）。
export default defineConfig(
  { ignores: ["src/gen/**", "node_modules/**", "test-results/**", "playwright-report/**"] },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  reactHooks.configs.flat.recommended,
  {
    languageOptions: {
      globals: globals.browser,
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    rules: {
      "@typescript-eslint/no-unused-vars": ["error", { argsIgnorePattern: "^_", varsIgnorePattern: "^_", ignoreRestSiblings: true }],
    },
  },
  {
    files: ["e2e/**", "vite.config.ts", "playwright.config.ts", "eslint.config.js"],
    languageOptions: { globals: globals.node },
  },
  {
    files: ["eslint.config.js", "e2e/**/*.mjs", "src/assets/heron-quick-node.user.js"],
    ...tseslint.configs.disableTypeChecked,
  },
  // 油猴脚本跑在脚本管理器给的页面环境里：GM_* 是管理器按 @grant 注入的只读全局，GM 是 Greasemonkey 4 的异步接口，
  // 脚本在 GM_* 缺席时以 typeof 守卫回退到它。
  {
    files: ["src/assets/heron-quick-node.user.js"],
    languageOptions: {
      globals: { GM_xmlhttpRequest: "readonly", GM_getValue: "readonly", GM_setValue: "readonly", GM_registerMenuCommand: "readonly", GM: "readonly" },
    },
  },
  // 服务替身写成 async () => value：它要返回 promise 才符合 ServiceImpl 的方法签名，函数体里没有 await 是这类
  // 替身的本来面目，不是漏写了 await。
  {
    files: ["**/*.test.{ts,tsx}", "src/test/**", "e2e/**"],
    rules: { "@typescript-eslint/require-await": "off" },
  },
  // e2e 断言的是真实 hub 的应答与真实页面的 DOM，"必有"只是用例对它们的预期：hub 漏了子消息或页面没渲染出元素时，
  // `!` 让用例在下游读 undefined / null 的属性时以 TypeError 失败，报告里看不出缺的是哪个方法的哪个字段、哪个元素。
  // 应答的子消息经 fixtures.ts 的 mustField 取出（缺席时抛出点名方法与字段的 MissingFieldError），DOM 与几何值显式判空后抛错。
  {
    files: ["e2e/**"],
    rules: { "@typescript-eslint/no-non-null-assertion": "error" },
  },
);
