/**
 * SPEC-W114 tests for lib/knowledge-upload.ts (pure module).
 * Run: apps/admin-web/scripts/test-knowledge-upload.sh
 * (tsc compiles the module to lib/.test-build/, node --test runs this file
 * against the compiled JS — no new package dependencies).
 *
 * REDIRECTION PROOFS (SPEC-W114):
 * - R1: allowlist check removed -> extension tests FAIL
 * - R2: size cap removed -> oversize test FAILS
 * - R3: reducer loses the failed transition -> failed-status test FAILS
 * - R4: async threshold removed -> mode-selection test FAILS
 */
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  ASYNC_THRESHOLD_BYTES,
  UPLOAD_EXTENSIONS,
  UPLOAD_MAX_BYTES,
  chooseUploadMode,
  fileExtension,
  pollDelayMs,
  reduceJobStatus,
  validateUploadFile,
} from "./.test-build/knowledge-upload.js";

test("allowlist accepts every W107 extension, case-insensitive", () => {
  for (const ext of UPLOAD_EXTENSIONS) {
    assert.deepEqual(validateUploadFile(`report.${ext}`, 1024), { ok: true });
    assert.deepEqual(validateUploadFile(`report.${ext.toUpperCase()}`, 1024), {
      ok: true,
    });
  }
});

test("unknown extension rejected with honest message", () => {
  const r = validateUploadFile("payload.exe", 1024);
  assert.equal(r.ok, false);
  assert.match(r.error, /Unsupported file type "\.exe"/);
  assert.match(r.error, /pdf/); // lists the allowed types
});

test("missing extension rejected", () => {
  const r = validateUploadFile("noextension", 1024);
  assert.equal(r.ok, false);
  assert.match(r.error, /no extension/);
});

test("10MB decoded cap: boundary passes, +1 byte fails", () => {
  assert.deepEqual(validateUploadFile("a.pdf", UPLOAD_MAX_BYTES), { ok: true });
  const r = validateUploadFile("a.pdf", UPLOAD_MAX_BYTES + 1);
  assert.equal(r.ok, false);
  assert.match(r.error, /limit is 10 MB/);
});

test("mode selection: <=3MB sync, >3MB async", () => {
  assert.equal(chooseUploadMode(0), "sync");
  assert.equal(chooseUploadMode(ASYNC_THRESHOLD_BYTES), "sync");
  assert.equal(chooseUploadMode(ASYNC_THRESHOLD_BYTES + 1), "async");
  assert.equal(chooseUploadMode(UPLOAD_MAX_BYTES), "async");
});

test("job reducer: pending/running -> processing", () => {
  assert.equal(reduceJobStatus("pending"), "processing");
  assert.equal(reduceJobStatus("running"), "processing");
});

test("job reducer: done -> done", () => {
  assert.equal(reduceJobStatus("done"), "done");
});

test("job reducer: failed -> failed (W112 reaper errors surface)", () => {
  assert.equal(reduceJobStatus("failed"), "failed");
});

test("job reducer: unknown status never crashes the dialog", () => {
  assert.equal(reduceJobStatus("cancelling"), "processing");
  assert.equal(reduceJobStatus(""), "processing");
});

test("poll backoff: exact early schedule + 10s cap", () => {
  assert.equal(pollDelayMs(0), 1000);
  assert.equal(pollDelayMs(1), 1500);
  assert.equal(pollDelayMs(2), 2000);
  assert.equal(pollDelayMs(3), 3000);
  assert.equal(pollDelayMs(9), 9000);
  assert.equal(pollDelayMs(10), 10_000);
  assert.equal(pollDelayMs(99), 10_000); // capped
});

test("fileExtension edge cases", () => {
  assert.equal(fileExtension("report.PDF"), "pdf");
  assert.equal(fileExtension("archive.tar.gz"), "gz");
  assert.equal(fileExtension("noext"), "");
  assert.equal(fileExtension(".hidden"), "hidden");
  assert.equal(fileExtension("trailing."), "");
});
