#!/bin/sh
# SPEC-W117 §4: compile the pure advanced-search module and run node --test.
# Mirrors scripts/test-chat-upload.sh (W115) byte-for-byte in idiom.
set -eu
cd "$(dirname "$0")/.."
rm -rf lib/.test-build
# TSC_BIN override: CI uses the project-local tsc; sandboxes without a full
# npm install can point at a standalone toolchain (same flags either way).
TSC_BIN="${TSC_BIN:-./node_modules/.bin/tsc}"
"$TSC_BIN" lib/advanced-search.ts --outDir lib/.test-build \
  --module nodenext --target es2022 --moduleResolution nodenext --strict --skipLibCheck
node --test lib/advanced-search.test.mjs
rm -rf lib/.test-build
