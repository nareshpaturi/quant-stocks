// Checks that the Worker in index.js started every workflow run it was due to
// start. The Worker fails silently when its GITHUB_TOKEN expires, so this runs
// from .github/workflows/scheduler-watchdog.yml on GitHub's own scheduler with
// the workflow's built-in token instead.
//
//   GITHUB_REPOSITORY=owner/repo GITHUB_TOKEN=... node src/watchdog.js
//
// Prints a report and exits 1 when a slot since the previous scheduled check
// (or in the last LOOKBACK_HOURS, when set) has no workflow_dispatch run of its
// workflow.

import { fileURLToPath } from "node:url";

import { SLOTS, easternSlot } from "./index.js";

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;

const WATCHDOG_WORKFLOW = "scheduler-watchdog.yml";

// A dispatch creates its run within seconds of the slot and the Worker's
// retries finish within a minute, so slots newer than this are not checked yet.
export const GRACE_MS = 15 * MINUTE;

// Lookback for the first scheduled check, with no earlier check to continue.
export const DEFAULT_LOOKBACK_MS = 30 * HOUR;

// Cap on the window after a long gap between checks, such as a weekend plus a
// dropped run.
export const MAX_LOOKBACK_MS = 96 * HOUR;

// checkFrom returns where this check's window starts. A scheduled check picks
// up where the previous scheduled check stopped, so each missed slot is
// reported once and the alert clears on the first check after a fix. A fixed
// lookback would re-report misses, because GitHub starts scheduled runs at
// irregular times.
export function checkFrom({ now, lookbackHours, previousCheck }) {
  if (lookbackHours) return now - lookbackHours * HOUR;
  if (previousCheck == null) return now - DEFAULT_LOOKBACK_MS;
  return Math.max(previousCheck - GRACE_MS, now - MAX_LOOKBACK_MS);
}

// expectedRuns lists the {time, slot, workflow} runs the Worker was due to
// start in [from, to], oldest first.
export function expectedRuns(from, to) {
  const runs = [];
  for (let time = Math.ceil(from / MINUTE) * MINUTE; time <= to; time += MINUTE) {
    const slot = easternSlot(new Date(time));
    if (!slot) continue;
    for (const { workflow } of SLOTS[slot]) runs.push({ time, slot, workflow });
  }
  return runs;
}

// missingRuns returns the expected runs with no run of their workflow created
// within GRACE_MS after the slot. dispatched holds {workflow, created} entries,
// with created in epoch milliseconds.
export function missingRuns(expected, dispatched) {
  return expected.filter(({ time, workflow }) =>
    !dispatched.some((run) =>
      run.workflow === workflow && run.created >= time && run.created <= time + GRACE_MS));
}

// previousCheck returns when the latest earlier scheduled watchdog run that
// completed its check was created, or null.
export function previousCheck(runs, currentRunId) {
  const checks = runs
    .filter((run) =>
      run.workflow === WATCHDOG_WORKFLOW &&
      run.event === "schedule" &&
      run.id !== currentRunId &&
      run.status === "completed" &&
      (run.conclusion === "success" || run.conclusion === "failure"))
    .map((run) => run.created);
  return checks.length ? Math.max(...checks) : null;
}

async function listRuns(repo, token, since) {
  const created = encodeURIComponent(`>=${new Date(since).toISOString()}`);
  const runs = [];
  for (let page = 1; ; page++) {
    const res = await fetch(
      `https://api.github.com/repos/${repo}/actions/runs?per_page=100&page=${page}&created=${created}`,
      {
        headers: {
          Accept: "application/vnd.github+json",
          Authorization: `Bearer ${token}`,
          "User-Agent": "quant-stocks-scheduler-watchdog",
          "X-GitHub-Api-Version": "2022-11-28",
        },
      },
    );
    if (!res.ok) throw new Error(`listing workflow runs failed: ${res.status} ${await res.text()}`);
    const { total_count: total, workflow_runs: batch } = await res.json();
    for (const run of batch) {
      runs.push({
        id: run.id,
        workflow: run.path.split("/").pop(),
        event: run.event,
        status: run.status,
        conclusion: run.conclusion,
        created: Date.parse(run.created_at),
      });
    }
    if (batch.length === 0 || runs.length >= total) return runs;
  }
}

const easternTime = new Intl.DateTimeFormat("en-US", {
  timeZone: "America/New_York",
  weekday: "short",
  month: "short",
  day: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

async function main() {
  const now = Date.now();
  const runs = await listRuns(process.env.GITHUB_REPOSITORY, process.env.GITHUB_TOKEN, now - MAX_LOOKBACK_MS);
  const from = checkFrom({
    now,
    lookbackHours: Number(process.env.LOOKBACK_HOURS),
    previousCheck: previousCheck(runs, Number(process.env.GITHUB_RUN_ID)),
  });
  const expected = expectedRuns(from, now - GRACE_MS);
  const dispatched = runs.filter((run) => run.event === "workflow_dispatch" && run.created >= from);
  const missing = missingRuns(expected, dispatched);
  const since = `${easternTime.format(from)} ET`;

  if (expected.length === 0) {
    console.log(`No slots were due since ${since}.`);
    return;
  }
  if (missing.length === 0) {
    console.log(`All ${expected.length} scheduled runs since ${since} started.`);
    return;
  }
  console.log(`The scheduler missed ${missing.length} of ${expected.length} runs since ${since}:`);
  console.log();
  for (const { time, workflow } of missing) {
    console.log(`  ${easternTime.format(time)} ET  ${workflow}`);
  }
  console.log(`
The Cloudflare Worker "quantstocks" in scheduler/ starts these runs. Usual causes:
  - Its GITHUB_TOKEN expired or was revoked. Regenerate the token at
    https://github.com/settings/personal-access-tokens, then from the repo root run
      npx wrangler@4 secret put GITHUB_TOKEN --config scheduler/wrangler.jsonc
  - Its cron triggers or the Worker itself were removed. Redeploy from scheduler/.
The Worker's cron events in the Cloudflare dashboard show each failure.`);
  process.exitCode = 1;
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main();
