import assert from "node:assert/strict";
import { test } from "node:test";

import worker, { SLOTS, easternSlot } from "./index.js";

// UTC fire times produced by the crons in wrangler.jsonc on a weekday.
const CRON_UTC = [
  [11, 30], [12, 30], [13, 30], [14, 30],
  [16, 0], [17, 0],
];

const env = {
  GITHUB_REPO: "owner/repo",
  WORKFLOW_FILE: "weekly-rebalance.yml",
  GITHUB_REF: "main",
  GITHUB_TOKEN: "test-token",
};

test("crons dispatch each ET slot exactly once per weekday across DST changes", () => {
  // Covers the Nov 2026 fall-back and Mar 2027 spring-forward transitions.
  const start = Date.UTC(2026, 9, 1);
  const end = Date.UTC(2027, 11, 31);
  let weekdays = 0;
  for (let day = start; day <= end; day += 86_400_000) {
    const weekday = new Date(day).getUTCDay();
    if (weekday === 0 || weekday === 6) continue;
    weekdays++;
    const fired = CRON_UTC
      .map(([h, m]) => easternSlot(new Date(day + (h * 60 + m) * 60_000)))
      .filter(Boolean);
    assert.deepEqual(fired, SLOTS, new Date(day).toISOString().slice(0, 10));
  }
  assert.ok(weekdays > 300);
});

test("weekends never match a slot", () => {
  // Saturday 2026-10-03 07:30 EDT and Sunday 2026-10-04 12:00 EDT.
  assert.equal(easternSlot(new Date("2026-10-03T11:30:00Z")), null);
  assert.equal(easternSlot(new Date("2026-10-04T16:00:00Z")), null);
});

test("dispatches the workflow on a slot", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => new Response(null, { status: 204 }));
  t.mock.method(console, "log", () => {});

  // Wednesday 2026-09-30 09:30 EDT.
  await worker.scheduled({ scheduledTime: Date.parse("2026-09-30T13:30:00Z") }, env);

  assert.equal(fetch.mock.callCount(), 1);
  const [url, init] = fetch.mock.calls[0].arguments;
  assert.equal(url, "https://api.github.com/repos/owner/repo/actions/workflows/weekly-rebalance.yml/dispatches");
  assert.equal(init.method, "POST");
  assert.equal(init.headers.Authorization, "Bearer test-token");
  assert.deepEqual(JSON.parse(init.body), { ref: "main" });
});

test("skips off-slot invocations", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => new Response(null, { status: 204 }));

  // 12:30 UTC is 08:30 EDT: the EST-only copy of the 07:30 cron.
  await worker.scheduled({ scheduledTime: Date.parse("2026-09-30T12:30:00Z") }, env);

  assert.equal(fetch.mock.callCount(), 0);
});

test("retries server errors", async (t) => {
  t.mock.timers.enable({ apis: ["setTimeout"] });
  t.mock.method(console, "warn", () => {});
  t.mock.method(console, "log", () => {});
  const statuses = [502, 204];
  const fetch = t.mock.method(globalThis, "fetch", async () => new Response(null, { status: statuses.shift() }));

  const run = worker.scheduled({ scheduledTime: Date.parse("2026-09-30T16:00:00Z") }, env);
  await new Promise(setImmediate);
  t.mock.timers.tick(5000);
  await run;

  assert.equal(fetch.mock.callCount(), 2);
});

test("does not retry a rejected token", async (t) => {
  const fetch = t.mock.method(globalThis, "fetch", async () => new Response("Bad credentials", { status: 401 }));

  await assert.rejects(
    worker.scheduled({ scheduledTime: Date.parse("2026-09-30T16:00:00Z") }, env),
    /workflow dispatch rejected: 401 Bad credentials/,
  );
  assert.equal(fetch.mock.callCount(), 1);
});
