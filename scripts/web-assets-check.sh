#!/bin/sh
# 面板与公开页构建产物的发布前核对，由 make web 在构建后调用：
# 1. 不得把字体内联成 data: URL：hub 对这两套产物下发的 CSP 是 default-src 'self'（internal/hub/web/web.go），
#    没有放行 font-src data:，内联字体会在浏览器里被静默拒绝、退回系统字体。
# 2. 预压缩副本（.gz / .br）不得有孤立文件、不得挂在不可压缩类型上、必须严格小于原文，且解压后必须与原文逐字节一致；
#    达到阈值（1 KiB）且可压出明显收益的 .js / .css 不得缺失 .gz 或 .br 副本，防止构建期预压缩静默失效。
set -eu

if [ $# -eq 0 ]; then
  repo=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
  set -- "$repo/internal/hub/web/dist" "$repo/internal/hub/web/dist-public"
fi

for dir in "$@"; do
  [ -d "$dir" ] || { echo "FAIL: $dir is missing; run the web build first" >&2; exit 1; }
done

node - "$@" <<'EOF'
const fs = require("node:fs");
const path = require("node:path");
const zlib = require("node:zlib");

const COMPRESSIBLE_EXTENSIONS = new Set([".js", ".mjs", ".css", ".html", ".svg", ".json", ".txt", ".xml"]);
const REQUIRED_SIDE_EXTENSIONS = new Set([".js", ".css"]);
const MIN_COMPRESS_BYTES = 1024;
const hasCompressionGain = (rawLen, compressedLen) => compressedLen < rawLen && compressedLen * 10 <= rawLen * 9;

const errors = [];

function walk(dir) {
  const files = [];
  for (const ent of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, ent.name);
    if (ent.isDirectory()) {
      files.push(...walk(full));
    } else if (ent.isFile()) {
      files.push(full);
    }
  }
  return files;
}

for (const dir of process.argv.slice(2)) {
  const files = walk(dir);
  const fileSet = new Set(files);

  for (const file of files) {
    if (file.endsWith(".gz") || file.endsWith(".br")) {
      const ext = file.endsWith(".gz") ? ".gz" : ".br";
      const orig = file.slice(0, -ext.length);
      if (!fileSet.has(orig)) {
        errors.push(`orphan precompressed asset without original: ${file}`);
        continue;
      }
      const origExt = path.extname(orig).toLowerCase();
      if (!COMPRESSIBLE_EXTENSIONS.has(origExt)) {
        errors.push(`unexpected precompressed sidecar for non-compressible asset: ${file}`);
        continue;
      }
      const raw = fs.readFileSync(orig);
      const compressed = fs.readFileSync(file);
      if (compressed.length >= raw.length) {
        errors.push(`precompressed asset is not smaller than original (${compressed.length} >= ${raw.length}): ${file}`);
        continue;
      }
      let restored;
      try {
        restored = ext === ".gz" ? zlib.gunzipSync(compressed) : zlib.brotliDecompressSync(compressed);
      } catch (err) {
        errors.push(`failed to decompress ${file}: ${err.message}`);
        continue;
      }
      if (Buffer.compare(restored, raw) !== 0) {
        errors.push(`decompressed bytes do not match original: ${file}`);
      }
      continue;
    }

    const raw = fs.readFileSync(file);
    if (raw.includes("data:font")) {
      errors.push(`built asset inlines a font as a data: URL, which the CSP default-src 'self' blocks: ${file}`);
    }
    const ext = path.extname(file).toLowerCase();
    if (REQUIRED_SIDE_EXTENSIONS.has(ext) && raw.length >= MIN_COMPRESS_BYTES) {
      const gz = zlib.gzipSync(raw, { level: 9 });
      if (hasCompressionGain(raw.length, gz.length) && !fileSet.has(`${file}.gz`)) {
        errors.push(`missing .gz precompressed asset for compressible file: ${file}`);
      }
      const br = zlib.brotliCompressSync(raw, {
        params: {
          [zlib.constants.BROTLI_PARAM_QUALITY]: 11,
          [zlib.constants.BROTLI_PARAM_MODE]: zlib.constants.BROTLI_MODE_TEXT,
          [zlib.constants.BROTLI_PARAM_SIZE_HINT]: raw.length,
        },
      });
      if (hasCompressionGain(raw.length, br.length) && !fileSet.has(`${file}.br`)) {
        errors.push(`missing .br precompressed asset for compressible file: ${file}`);
      }
    }
  }
}

if (errors.length > 0) {
  for (const e of errors) console.error(`FAIL: ${e}`);
  process.exit(1);
}
console.log("web assets ok: no data: fonts and precompressed assets verified");
EOF
