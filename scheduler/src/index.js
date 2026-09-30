// Starts the Weekly Portfolio Rebalance workflow at exact Eastern wall-clock
// times. GitHub's own `schedule` trigger queues runs by hours under load, while
// a workflow_dispatch call creates the run immediately.
//
// Cloudflare cron triggers run on UTC only, so wrangler.jsonc fires at both the
// EDT and EST equivalent of every slot. This handler keeps only the invocation
// whose America/New_York local time matches a slot, which keeps the schedule
// correct across daylight-saving changes without editing crons twice a year.

export const SLOTS = ["07:30", "09:30", "12:00"];

const easternClock = new Intl.DateTimeFormat("en-US", {
  timeZone: "America/New_York",
  weekday: "short",
  hour: "2-digit",
  minute: "2-digit",
  hourCycle: "h23",
});

// easternSlot returns the slot ("HH:MM") that the given instant falls on in New
// York, or null when it is not a weekday slot.
export function easternSlot(date) {
  const parts = Object.fromEntries(
    easternClock.formatToParts(date).map((part) => [part.type, part.value]),
  );
  if (parts.weekday === "Sat" || parts.weekday === "Sun") return null;
  const slot = `${parts.hour}:${parts.minute}`;
  return SLOTS.includes(slot) ? slot : null;
}

const MAX_ATTEMPTS = 3;

async function dispatchWorkflow(env) {
  const url =
    `https://api.github.com/repos/${env.GITHUB_REPO}` +
    `/actions/workflows/${env.WORKFLOW_FILE}/dispatches`;

  for (let attempt = 1; attempt <= MAX_ATTEMPTS; attempt++) {
    const res = await fetch(url, {
      method: "POST",
      headers: {
        Accept: "application/vnd.github+json",
        Authorization: `Bearer ${env.GITHUB_TOKEN}`,
        "Content-Type": "application/json",
        "User-Agent": "quant-stocks-scheduler",
        "X-GitHub-Api-Version": "2022-11-28",
      },
      body: JSON.stringify({ ref: env.GITHUB_REF }),
    });
    if (res.ok) return;

    const detail = `${res.status} ${await res.text()}`;
    // 4xx other than rate limiting means a bad token, repo, or workflow name;
    // retrying cannot fix it.
    if (res.status < 500 && res.status !== 429) {
      throw new Error(`workflow dispatch rejected: ${detail}`);
    }
    console.warn(`workflow dispatch attempt ${attempt} failed: ${detail}`);
    if (attempt < MAX_ATTEMPTS) {
      await new Promise((resolve) => setTimeout(resolve, attempt * 5000));
    }
  }
  throw new Error(`workflow dispatch failed after ${MAX_ATTEMPTS} attempts`);
}

export default {
  async scheduled(controller, env) {
    // scheduledTime is the cron's nominal minute, so a trigger that fires a few
    // seconds late still matches its slot.
    const slot = easternSlot(new Date(controller.scheduledTime));
    if (!slot) return;
    await dispatchWorkflow(env);
    console.log(`dispatched ${env.WORKFLOW_FILE} on ${env.GITHUB_REF} for ${slot} ET`);
  },
};
