#!/bin/sh
# SPEC-W115 C7: compile the pure chat-upload module and run node --test.
# Mirrors scripts/test-knowledge-upload.sh (W114) byte-for-byte in idiom.
set -eu
cd "$(dirname "$0")/.."
rm -rf lib/.test-build
# TSC_BIN override: CI uses the project-local tsc; sandboxes without a full
# npm install can point at a standalone toolchain (same flags either way).
TSC_BIN="${TSC_BIN:-./node_modules/.bin/tsc}"
"$TSC_BIN" lib/chat-upload.ts --outDir lib/.test-build \
  --module nodenext --target es2022 --moduleResolution nodenext --strict --skipLibCheck
node --test lib/chat-upload.test.mjs
rm -rf lib/.test-build
