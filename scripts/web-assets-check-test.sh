#!/bin/sh
# scripts/web-assets-check.sh 的回归检查（make script-test 调用）：
# 在临时产物目录里构造正反例，验证字体内联检查与预压缩一致性的每条守卫都能真红。
set -eu

repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
check="$repo/scripts/web-assets-check.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
failures=0

make_valid_fixture() {
  dir=$1
  mkdir -p "$dir/assets"
  node - "$dir" <<'EOF'
const fs = require("node:fs");
const path = require("node:path");
const zlib = require("node:zlib");

const dir = process.argv[2];
const js = Buffer.from("export const data = '" + "heron-static-asset-".repeat(120) + "';\n");
const css = Buffer.from("body{" + "margin:0;padding:0;".repeat(100) + "}");
const html = Buffer.from("<!doctype html><div id=root></div>");
const woff2 = Buffer.from("wOF2" + "\x00\x01\x02\x03".repeat(400));

fs.writeFileSync(path.join(dir, "index.html"), html);
fs.writeFileSync(path.join(dir, "assets", "app.js"), js);
fs.writeFileSync(path.join(dir, "assets", "app.js.gz"), zlib.gzipSync(js, { level: 9 }));
fs.writeFileSync(path.join(dir, "assets", "app.js.br"), zlib.brotliCompressSync(js));
fs.writeFileSync(path.join(dir, "assets", "app.css"), css);
fs.writeFileSync(path.join(dir, "assets", "app.css.gz"), zlib.gzipSync(css, { level: 9 }));
fs.writeFileSync(path.join(dir, "assets", "app.css.br"), zlib.brotliCompressSync(css));
fs.writeFileSync(path.join(dir, "assets", "font.woff2"), woff2);
EOF
}

expect_ok() {
  name=$1
  shift
  if ! "$check" "$@" > "$work/out" 2>&1; then
    echo "FAIL: $name expected ok, got failure:" >&2
    sed 's/^/    /' "$work/out" >&2
    failures=$((failures + 1))
  fi
}

expect_fail() {
  name=$1
  want=$2
  shift 2
  if "$check" "$@" > "$work/out" 2>&1; then
    echo "FAIL: $name expected failure ($want), got exit 0" >&2
    failures=$((failures + 1))
    return
  fi
  if ! grep -q "$want" "$work/out"; then
    echo "FAIL: $name failed without expected message '$want':" >&2
    sed 's/^/    /' "$work/out" >&2
    failures=$((failures + 1))
  fi
}

# 1. 合法产物通过
make_valid_fixture "$work/ok"
expect_ok "valid fixture" "$work/ok"

# 2. 空目录（替身构建场景）通过
mkdir -p "$work/empty"
expect_ok "empty directory" "$work/empty"

# 3. 缺失目录失败
expect_fail "missing directory" "is missing" "$work/does-not-exist"

# 4. 内联 data:font 失败
make_valid_fixture "$work/datafont"
printf 'url(data:font/woff2;base64,AAAA)' >> "$work/datafont/assets/app.css"
node -e 'const fs=require("fs"),z=require("zlib"),b=fs.readFileSync(process.argv[1]);fs.writeFileSync(process.argv[1]+".gz",z.gzipSync(b,{level:9}));fs.writeFileSync(process.argv[1]+".br",z.brotliCompressSync(b));' "$work/datafont/assets/app.css"
expect_fail "inlined data:font" "inlines a font as a data: URL" "$work/datafont"

# 5. .gz 解压内容与原文不符（合法 gzip 但压的是另一份内容）
make_valid_fixture "$work/gz-mismatch"
node -e 'const fs=require("fs"),z=require("zlib");fs.writeFileSync(process.argv[1],z.gzipSync(Buffer.from("tampered")));' "$work/gz-mismatch/assets/app.js.gz"
expect_fail "gzip content mismatch" "decompressed bytes do not match original" "$work/gz-mismatch"

# 6. .gz 损坏
make_valid_fixture "$work/gz-corrupt"
printf 'not-valid-gzip' > "$work/gz-corrupt/assets/app.js.gz"
expect_fail "gzip corrupt" "failed to decompress" "$work/gz-corrupt"

# 7. .br 解压内容与原文不符
make_valid_fixture "$work/br-mismatch"
node -e 'const fs=require("fs"),z=require("zlib");fs.writeFileSync(process.argv[1],z.brotliCompressSync(Buffer.from("tampered")));' "$work/br-mismatch/assets/app.js.br"
expect_fail "brotli content mismatch" "decompressed bytes do not match original" "$work/br-mismatch"

# 8. .br 损坏
make_valid_fixture "$work/br-corrupt"
printf '\377\377\377\377not-valid-br' > "$work/br-corrupt/assets/app.js.br"
expect_fail "brotli corrupt" "failed to decompress" "$work/br-corrupt"

# 9. 孤立的预压缩文件（无原文）
make_valid_fixture "$work/orphan"
rm "$work/orphan/assets/app.css"
expect_fail "orphan sidecar" "orphan precompressed asset" "$work/orphan"

# 10. 不可压缩格式挂了预压缩副本
make_valid_fixture "$work/noncompressible"
node -e 'const fs=require("fs"),z=require("zlib"),b=fs.readFileSync(process.argv[1]);fs.writeFileSync(process.argv[1]+".gz",z.gzipSync(b,{level:9}));' "$work/noncompressible/assets/font.woff2"
expect_fail "non-compressible sidecar" "non-compressible asset" "$work/noncompressible"

# 11. 压缩结果未小于原文
make_valid_fixture "$work/not-smaller"
node -e 'const fs=require("fs"),z=require("zlib");const raw=Buffer.from("short");fs.writeFileSync(process.argv[1],raw);fs.writeFileSync(process.argv[1]+".gz",z.gzipSync(raw));' "$work/not-smaller/assets/tiny.js"
expect_fail "compressed not smaller" "not smaller than original" "$work/not-smaller"

# 12. 达到阈值的可压缩 JS 缺失 .gz / .br
make_valid_fixture "$work/missing-gz"
rm "$work/missing-gz/assets/app.js.gz"
expect_fail "missing gz" "missing .gz precompressed asset" "$work/missing-gz"

make_valid_fixture "$work/missing-br"
rm "$work/missing-br/assets/app.js.br"
expect_fail "missing br" "missing .br precompressed asset" "$work/missing-br"

if [ "$failures" -ne 0 ]; then
  echo "web-assets-check-test: $failures failure(s)" >&2
  exit 1
fi
echo "web-assets-check-test ok"
