// Checks that the Worker in index.js started every workflow run it was due to
// start. The Worker fails silently when its GITHUB_TOKEN expires, so this runs
// from .github/workflows/scheduler-watchdog.yml on GitHub's own scheduler with
// the workflow's built-in token instead.
//
//   GITHUB_REPOSITORY=owner/repo GITHUB_TOKEN=... node src/watchdog.js
//
// Prints a report and exits 1 when a slot in the last LOOKBACK_HOURS (default
// 30) has no workflow_dispatch run of its workflow.

import { fileURLToPath } from "node:url";

import { SLOTS, easternSlot } from "./index.js";

const MINUTE = 60_000;

// A dispatch creates its run within seconds of the slot and the Worker's
// retries finish within a minute, so slots newer than this are not checked yet.
export const GRACE_MS = 15 * MINUTE;

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

async function dispatchedRuns(repo, token, since) {
  const created = encodeURIComponent(`>=${new Date(since).toISOString()}`);
  const res = await fetch(
    `https://api.github.com/repos/${repo}/actions/runs?event=workflow_dispatch&per_page=100&created=${created}`,
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
  const { workflow_runs: runs } = await res.json();
  return runs.map((run) => ({
    workflow: run.path.split("/").pop(),
    created: Date.parse(run.created_at),
  }));
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
  const from = now - Number(process.env.LOOKBACK_HOURS || 30) * 60 * MINUTE;
  const expected = expectedRuns(from, now - GRACE_MS);
  const dispatched = await dispatchedRuns(process.env.GITHUB_REPOSITORY, process.env.GITHUB_TOKEN, from);
  const missing = missingRuns(expected, dispatched);

  if (missing.length === 0) {
    console.log(`All ${expected.length} scheduled runs since ${easternTime.format(from)} ET started.`);
    return;
  }
  console.log(`The scheduler missed ${missing.length} of ${expected.length} runs since ${easternTime.format(from)} ET:`);
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
