import test from "node:test";
import assert from "node:assert/strict";
import {
  CHAT_UPLOAD_MAX_BYTES,
  validateChatUpload,
  fileExtension,
  verdictChip,
  verdictSummary,
} from "./.test-build/chat-upload.js";

test("fileExtension lowercases and handles edge cases", () => {
  assert.equal(fileExtension("Stub.PDF"), "pdf");
  assert.equal(fileExtension("noext"), "");
  assert.equal(fileExtension("a.tar.gz"), "gz");
});

test("accepts the five caller formats", () => {
  for (const name of ["a.jpg", "a.jpeg", "a.png", "a.webp", "a.pdf"]) {
    assert.deepEqual(validateChatUpload({ name, size: 100 }), { ok: true });
  }
});

test("rejects unknown extensions with a helpful message", () => {
    const r = validateChatUpload({ name: "a.gif", size: 100 });
  assert.equal(r.ok, false);
  assert.match(r.error, /jpg, png, webp/);
});

test("rejects empty and oversize files", () => {
  assert.equal(validateChatUpload({ name: "a.png", size: 0 }).ok, false);
  const r = validateChatUpload({ name: "a.png", size: CHAT_UPLOAD_MAX_BYTES + 1 });
  assert.equal(r.ok, false);
  assert.match(r.error, /10 MB/);
  // boundary: exactly the cap passes
  assert.equal(validateChatUpload({ name: "a.png", size: CHAT_UPLOAD_MAX_BYTES }).ok, true);
});

test("verdictChip maps the three verdicts + unknown", () => {
  assert.deepEqual(verdictChip("validated"), { label: "Checks passed", tone: "success" });
  assert.deepEqual(verdictChip("needs_review"), { label: "Under review", tone: "warning" });
  assert.deepEqual(verdictChip("rejected"), { label: "Not accepted", tone: "danger" });
  assert.deepEqual(verdictChip("???"), { label: "Received", tone: "neutral" });
});

test("verdictSummary validated mentions checks", () => {
  assert.equal(
    verdictSummary("pay_stub", "validated", []),
    "Your pay stub was received and passed all consistency checks.",
  );
});

test("verdictSummary rejected humanizes the first reason", () => {
  assert.equal(
    verdictSummary("pay_stub", "rejected", ["net_exceeds_gross"]),
    "We could not accept this pay stub: the net pay is higher than the gross pay.",
  );
  assert.equal(
    verdictSummary("document", "rejected", ["missing_required_field:employer_name"]),
    "We could not accept this document: the employer name is missing.",
  );
});

test("verdictSummary labels receipt (SPEC-W116)", () => {
  assert.equal(
    verdictSummary("receipt", "validated", []),
    "Your receipt was received and passed all consistency checks.",
  );
});

test("verdictSummary needs_review and fallback", () => {
  assert.equal(
    verdictSummary("document", "needs_review", []),
    "Your document was received; a team member will review it shortly.",
  );
});
