import assert from "node:assert/strict";
import { test } from "node:test";

import {
  DEFAULT_LOOKBACK_MS,
  GRACE_MS,
  MAX_LOOKBACK_MS,
  checkFrom,
  expectedRuns,
  missingRuns,
  previousCheck,
} from "./watchdog.js";

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

test("a scheduled check continues from the previous scheduled check", () => {
  const now = at("2026-11-11T21:34:00Z");
  const previous = at("2026-11-11T01:14:00Z");

  assert.equal(checkFrom({ now, previousCheck: previous }), previous - GRACE_MS);
  assert.equal(checkFrom({ now, previousCheck: null }), now - DEFAULT_LOOKBACK_MS);
  assert.equal(checkFrom({ now, previousCheck: now - 7 * 86_400_000 }), now - MAX_LOOKBACK_MS);
  assert.equal(checkFrom({ now, lookbackHours: 2, previousCheck: previous }), now - 2 * 3_600_000);
});

test("only completed scheduled watchdog checks count as previous checks", () => {
  const watchdog = { workflow: "scheduler-watchdog.yml", event: "schedule", status: "completed", conclusion: "failure" };
  const runs = [
    { ...watchdog, id: 1, created: 100 },
    { ...watchdog, id: 2, created: 200, conclusion: "success" },
    { ...watchdog, id: 3, created: 300, conclusion: "cancelled" },
    { ...watchdog, id: 4, created: 400, event: "workflow_dispatch" },
    { ...watchdog, id: 5, created: 500, status: "in_progress", conclusion: null },
    { ...watchdog, id: 6, created: 600, workflow: "weekly-rebalance.yml" },
    { ...watchdog, id: 7, created: 700 },
  ];

  assert.equal(previousCheck(runs, 7), 200);
  assert.equal(previousCheck(runs.slice(2), 7), null);
});
