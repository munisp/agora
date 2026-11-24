import test from "node:test";
import assert from "node:assert/strict";
import {
  EMPTY_SEARCH_STATE,
  buildSearchParams,
  validateDateRange,
  describeChips,
  clampLimit,
  nextOffset,
  prevOffset,
} from "./.test-build/advanced-search.js";

test("buildSearchParams omits empties and keeps paging", () => {
  const p = buildSearchParams(EMPTY_SEARCH_STATE);
  assert.equal(p.get("q"), null);
  assert.equal(p.get("channel"), null);
  assert.equal(p.get("limit"), "25");
  assert.equal(p.get("offset"), "0");
});

test("buildSearchParams sets filters and inclusive ISO day bounds", () => {
  const p = buildSearchParams({
    ...EMPTY_SEARCH_STATE,
    q: "  pay stub ",
    channel: "whatsapp",
    role: "user",
    fromTs: "2026-11-01",
    toTs: "2026-11-24",
  });
  assert.equal(p.get("q"), "pay stub");
  assert.equal(p.get("channel"), "whatsapp");
  assert.equal(p.get("role"), "user");
  assert.equal(p.get("from_ts"), "2026-11-01T00:00:00Z");
  assert.equal(p.get("to_ts"), "2026-11-24T23:59:59Z");
});

test("validateDateRange accepts empty and ordered ranges", () => {
  assert.equal(validateDateRange("", ""), null);
  assert.equal(validateDateRange("2026-11-01", "2026-11-24"), null);
  assert.equal(validateDateRange("2026-11-01", ""), null);
});

test("validateDateRange rejects inverted and malformed ranges", () => {
  assert.match(validateDateRange("2026-11-24", "2026-11-01") ?? "", /after/);
  assert.match(validateDateRange("11/01/2026", "") ?? "", /yyyy-mm-dd/);
  assert.match(validateDateRange("", "2026-1-1") ?? "", /yyyy-mm-dd/);
});

test("describeChips reflects active filters only", () => {
  assert.deepEqual(describeChips(EMPTY_SEARCH_STATE), []);
  const chips = describeChips({
    ...EMPTY_SEARCH_STATE, q: "x", channel: "web", toTs: "2026-11-24",
  });
  assert.deepEqual(chips.map((c) => c.key), ["q", "channel", "toTs"]);
});

test("clampLimit bounds to 1..100", () => {
  assert.equal(clampLimit(0), 1);
  assert.equal(clampLimit(500), 100);
  assert.equal(clampLimit(25), 25);
});

test("nextOffset/prevOffset paging clamps", () => {
  const s = { ...EMPTY_SEARCH_STATE, offset: 0 };
  assert.equal(nextOffset(s, 100), 25);
  assert.equal(nextOffset({ ...s, offset: 75 }, 100), null);
  assert.equal(prevOffset(s), null);
  assert.equal(prevOffset({ ...s, offset: 10 }), 0);
});

test("nextOffset respects the result window", () => {
  const s = { ...EMPTY_SEARCH_STATE, offset: 9975, limit: 25 };
  assert.equal(nextOffset(s, 1e9), null);
});
