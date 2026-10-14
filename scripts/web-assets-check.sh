#!/bin/sh
# 面板与公开页的构建产物不得把字体内联成 data: URL：hub 对这两套产物下发的 CSP 是 default-src 'self'
# （internal/hub/web/web.go），没有放行 font-src data:，内联的字体会在访客浏览器里被静默拒绝、退回系统字体。
# 不内联由 web/vite.config.ts 的 assetsInlineLimit 保证；这里核对产物本身，配置被改掉时在构建期失败，而不是在访客端。
set -eu
cd "$(dirname "$0")/.."
for dir in internal/hub/web/dist internal/hub/web/dist-public; do
  [ -d "$dir" ] || { echo "FAIL: $dir is missing; run the web build first"; exit 1; }
done
found=$(grep -rl 'data:font' internal/hub/web/dist internal/hub/web/dist-public || true)
if [ -n "$found" ]; then
  printf 'FAIL: built assets inline a font as a data: URL, which the CSP default-src '"'"'self'"'"' blocks:\n%s\n' "$found"
  exit 1
fi
echo "web assets ok: no data: fonts"
