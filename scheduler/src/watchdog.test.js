import assert from "node:assert/strict";
import { test } from "node:test";

import { GRACE_MS, expectedRuns, missingRuns } from "./watchdog.js";

const at = (iso) => Date.parse(iso);

test("expects every slot of a weekday in EST", () => {
  // Tuesday 2026-11-10, EST (UTC-5).
  const runs = expectedRuns(at("2026-11-10T05:00:00Z"), at("2026-11-11T04:59:00Z"));

  assert.deepEqual(
    runs.map(({ time, slot, workflow }) => [new Date(time).toISOString(), slot, workflow]),
    [
      ["2026-11-10T12:30:00.000Z", "07:30", "weekly-rebalance.yml"],
      ["2026-11-10T14:25:00.000Z", "09:25", "overnight-hold.yml"],
      ["2026-11-10T14:30:00.000Z", "09:30", "weekly-rebalance.yml"],
      ["2026-11-10T17:00:00.000Z", "12:00", "weekly-rebalance.yml"],
      ["2026-11-10T20:55:00.000Z", "15:55", "overnight-hold.yml"],
    ],
  );
});

test("expects nothing over a weekend", () => {
  // Saturday 2026-11-07 through Sunday 2026-11-08, New York time.
  assert.deepEqual(expectedRuns(at("2026-11-07T05:00:00Z"), at("2026-11-09T04:59:00Z")), []);
});

test("a run counts only for its own workflow within the grace period", () => {
  const slot = at("2026-11-10T12:30:00Z");
  const expected = [{ time: slot, slot: "07:30", workflow: "weekly-rebalance.yml" }];

  assert.deepEqual(missingRuns(expected, [{ workflow: "weekly-rebalance.yml", created: slot + 20_000 }]), []);
  assert.deepEqual(missingRuns(expected, [{ workflow: "weekly-rebalance.yml", created: slot + GRACE_MS }]), []);
  assert.deepEqual(missingRuns(expected, [{ workflow: "weekly-rebalance.yml", created: slot + GRACE_MS + 1 }]), expected);
  assert.deepEqual(missingRuns(expected, [{ workflow: "weekly-rebalance.yml", created: slot - 1 }]), expected);
  assert.deepEqual(missingRuns(expected, [{ workflow: "overnight-hold.yml", created: slot + 20_000 }]), expected);
});
