import test from "node:test";
import assert from "node:assert/strict";
import {
  parseShownDate,
  parseShownNumber,
  sortKeyOf,
  sortedOrder,
  isWhenHeader,
  mostlyDates,
} from "./.test-build/table-sort.js";

const fmt = (iso) =>
  new Intl.DateTimeFormat("en-US", { dateStyle: "medium", timeStyle: "short", timeZone: "Africa/Lagos" }).format(new Date(iso));

test("dates as the dashboard prints them parse in order", () => {
  const a = parseShownDate(fmt("2026-11-26T09:05:00Z"));
  const b = parseShownDate(fmt("2026-11-26T14:30:00Z"));
  const c = parseShownDate(fmt("2026-12-01T08:00:00Z"));
  assert.ok(a !== null && b !== null && c !== null, "formatDateTime output parses");
  assert.ok(a < b && b < c);
  assert.ok(parseShownDate("Nov 26, 2026") < parseShownDate("Nov 27, 2026"));
  assert.ok(parseShownDate("26 Nov 2026") < parseShownDate("1 Dec 2026"));
  assert.ok(parseShownDate("11/26/2026, 10:30:00 AM") < parseShownDate("11/26/2026, 1:00:00 PM"));
  assert.ok(parseShownDate("2026-11-26T10:00:00Z") < parseShownDate("2026-11-26T11:00:00Z"));
  assert.ok(parseShownDate("26/11/2026, 10:30:00") > parseShownDate("25/11/2026, 23:00:00"), "day-first dates");
  assert.ok(parseShownDate("9:00 AM") < parseShownDate("12:30 PM"));
  assert.equal(parseShownDate("12:00 AM"), 0);
  assert.equal(parseShownDate("Leadway Pensure"), null);
  assert.equal(parseShownDate("May contain 2026"), null);
});

test("numbers as shown", () => {
  assert.equal(parseShownNumber("₦12,500.00"), 12500);
  assert.equal(parseShownNumber("NGN 1,200"), 1200);
  assert.equal(parseShownNumber("45%"), 45);
  assert.equal(parseShownNumber("-3"), -3);
  assert.equal(parseShownNumber("12 bookings"), null);
});

test("empties sort last both ways; text is natural order", () => {
  const keys = ["b", "—", "a10", "a2", ""].map(sortKeyOf);
  assert.deepEqual(sortedOrder(keys, "asc"), [3, 2, 0, 1, 4]);
  assert.deepEqual(sortedOrder(keys, "desc"), [0, 2, 3, 1, 4]);
});

test("latest first on a when column", () => {
  const shown = [fmt("2026-11-20T10:00:00Z"), fmt("2026-11-26T10:00:00Z"), fmt("2026-11-22T10:00:00Z")];
  assert.ok(mostlyDates(shown));
  assert.deepEqual(sortedOrder(shown.map(sortKeyOf), "desc"), [1, 2, 0]);
  assert.ok(!mostlyDates(["Bisi", "Chika", "—"]));
  assert.ok(isWhenHeader("When") && isWhenHeader("Created") && isWhenHeader("Last seen") && isWhenHeader("Started at"));
  assert.ok(isWhenHeader("Captured") && isWhenHeader("Reported") && isWhenHeader("Added"));
  assert.ok(!isWhenHeader("Name") && !isWhenHeader("Status") && !isWhenHeader("Lateness"));
});
