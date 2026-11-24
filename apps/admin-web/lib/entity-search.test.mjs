// SPEC-W119 §5: node --test coverage for lib/entity-search.ts — params
// building (descriptor allow-list, date handling), chips, clearing, date
// validation, offset math, and the three shipped descriptors.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  BOOKINGS_DESCRIPTOR,
  CONTACTS_DESCRIPTOR,
  LEADS_DESCRIPTOR,
  ENTITY_MAX_LIMIT,
  ENTITY_MAX_WINDOW,
  ENTITY_PAGE_SIZE,
  emptyEntitySearchState,
  clampEntityLimit,
  buildEntitySearchParams,
  validateEntityDateRange,
  describeEntityChips,
  clearEntityChip,
  nextEntityOffset,
  prevEntityOffset,
} from "./.test-build/entity-search.js";

test("bookings params: q + selects + inclusive date bounds", () => {
  const state = {
    ...emptyEntitySearchState(),
    q: "  ada  ",
    filters: { status: "confirmed", source: "web" },
    fromTs: "2026-01-02",
    toTs: "2026-01-31",
    offset: 25,
  };
  const p = buildEntitySearchParams(state, BOOKINGS_DESCRIPTOR);
  assert.equal(p.get("q"), "ada");
  assert.equal(p.get("status"), "confirmed");
  assert.equal(p.get("source"), "web");
  assert.equal(p.get("from_ts"), "2026-01-02T00:00:00Z");
  assert.equal(p.get("to_ts"), "2026-01-31T23:59:59Z");
  assert.equal(p.get("limit"), String(ENTITY_PAGE_SIZE));
  assert.equal(p.get("offset"), "25");
});

test("descriptor allow-list: unknown filter keys are ignored", () => {
  const state = {
    ...emptyEntitySearchState(),
    filters: { status: "new", channel: "whatsapp", hacker: "x", source: "web" },
  };
  const p = buildEntitySearchParams(state, LEADS_DESCRIPTOR);
  assert.equal(p.get("status"), "new");
  assert.equal(p.get("channel"), "whatsapp");
  assert.equal(p.get("hacker"), null);
  assert.equal(p.get("source"), null); // not a leads filter
});

test("contacts: no dates are ever sent", () => {
  const state = {
    ...emptyEntitySearchState(),
    filters: { source: "crm" },
    fromTs: "2026-01-01",
    toTs: "2026-02-01",
  };
  const p = buildEntitySearchParams(state, CONTACTS_DESCRIPTOR);
  assert.equal(p.get("source"), "crm");
  assert.equal(p.get("from_ts"), null);
  assert.equal(p.get("to_ts"), null);
  assert.equal(validateEntityDateRange(CONTACTS_DESCRIPTOR, "junk", "worse"), null);
});

test("date validation mirrors the W117 messages", () => {
  assert.equal(
    validateEntityDateRange(BOOKINGS_DESCRIPTOR, "2026-02-01", "2026-01-01"),
    "Start date must not be after the end date.",
  );
  assert.equal(
    validateEntityDateRange(LEADS_DESCRIPTOR, "bad", ""),
    "Start date must be yyyy-mm-dd.",
  );
  assert.equal(validateEntityDateRange(LEADS_DESCRIPTOR, "", "2026-01-01"), null);
});

test("limit clamps to [1, 100]; offset floors at 0", () => {
  assert.equal(clampEntityLimit(0), 1);
  assert.equal(clampEntityLimit(500), ENTITY_MAX_LIMIT);
  assert.equal(clampEntityLimit(25.7), 25);
  const p = buildEntitySearchParams(
    { ...emptyEntitySearchState(), limit: 500, offset: -4 },
    BOOKINGS_DESCRIPTOR,
  );
  assert.equal(p.get("limit"), "100");
  assert.equal(p.get("offset"), "0");
});

test("chips describe active filters per descriptor", () => {
  const state = {
    ...emptyEntitySearchState(),
    q: "ada",
    filters: { status: "new", channel: "qr" },
    fromTs: "2026-01-01",
  };
  const chips = describeEntityChips(state, LEADS_DESCRIPTOR);
  assert.deepEqual(
    chips.map((c) => c.key),
    ["q", "status", "channel", "fromTs"],
  );
  assert.equal(chips[0].label, '"ada"');
  assert.equal(chips[1].label, "Status: new");
  // Contacts never produce date chips.
  const contactChips = describeEntityChips(
    { ...state, filters: { source: "crm" } },
    CONTACTS_DESCRIPTOR,
  );
  assert.deepEqual(
    contactChips.map((c) => c.key),
    ["q", "source"],
  );
});

test("clearEntityChip clears exactly one key", () => {
  const state = {
    ...emptyEntitySearchState(),
    q: "ada",
    filters: { status: "new", channel: "qr" },
    fromTs: "2026-01-01",
  };
  assert.equal(clearEntityChip(state, LEADS_DESCRIPTOR, "q").q, "");
  assert.equal(
    clearEntityChip(state, LEADS_DESCRIPTOR, "channel").filters.channel,
    "",
  );
  assert.equal(clearEntityChip(state, LEADS_DESCRIPTOR, "fromTs").fromTs, "");
  // Unknown keys are a no-op; contacts ignore date keys.
  assert.equal(clearEntityChip(state, LEADS_DESCRIPTOR, "nope"), state);
  assert.equal(
    clearEntityChip(state, CONTACTS_DESCRIPTOR, "fromTs").fromTs,
    "2026-01-01",
  );
});

test("offset math respects total and the result window", () => {
  const state = { ...emptyEntitySearchState(), offset: 0 };
  assert.equal(nextEntityOffset(state, 100), 25);
  assert.equal(nextEntityOffset({ ...state, offset: 75 }, 100), null);
  assert.equal(
    nextEntityOffset({ ...state, offset: ENTITY_MAX_WINDOW - 25 }, 99999),
    null,
  );
  assert.equal(prevEntityOffset({ ...state, offset: 50 }), 25);
  assert.equal(prevEntityOffset({ ...state, offset: 10 }), 0);
  assert.equal(prevEntityOffset(state), null);
});

test("the three descriptors carry the documented shapes", () => {
  assert.equal(BOOKINGS_DESCRIPTOR.dateField, "starts_at");
  assert.deepEqual(
    BOOKINGS_DESCRIPTOR.selects.map((s) => s.key),
    ["status", "source"],
  );
  assert.equal(CONTACTS_DESCRIPTOR.dateField, null);
  assert.deepEqual(
    CONTACTS_DESCRIPTOR.selects.map((s) => s.key),
    ["source"],
  );
  assert.equal(LEADS_DESCRIPTOR.dateField, "created_at");
  assert.deepEqual(
    LEADS_DESCRIPTOR.selects.map((s) => s.key),
    ["status", "channel"],
  );
  // Lead channels mirror the backend CHECK constraint (10 values).
  assert.equal(LEADS_DESCRIPTOR.selects[1].options.length, 10);
});
