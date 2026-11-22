#!/bin/sh
# SPEC-W114: test the knowledge-upload pure module with zero new package
# deps — tsc compiles the .ts module to JS, node --test runs the .mjs test
# against it. Run from apps/admin-web/:  sh scripts/test-knowledge-upload.sh
set -eu
cd "$(dirname "$0")/.."

rm -rf lib/.test-build
./node_modules/.bin/tsc lib/knowledge-upload.ts \
  --outDir lib/.test-build \
  --module nodenext \
  --target es2022 \
  --moduleResolution nodenext \
  --strict \
  --skipLibCheck
node --test lib/knowledge-upload.test.mjs
rm -rf lib/.test-build
echo "knowledge-upload tests OK"
