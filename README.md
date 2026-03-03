# quant-stocks

A weekly stock portfolio rebalancer that runs automatically as a GitHub Action every Monday morning. It connects to your Tradier brokerage account, fetches the latest momentum rankings from the QuantMyStocks leaderboard API, and executes the minimum set of trades needed to keep your portfolio aligned with the strategy.

You configure everything through GitHub Secrets and Variables — no code changes required.

---

## How the strategy works

The rebalancer uses a **slack-based momentum strategy**. You configure three numbers per portfolio:

- **`MAX_STOCKS`** — target number of holdings (e.g. 5)
- **`SLACK_VALUE`** — a buffer that prevents unnecessary selling (e.g. 2)
- **`INITIAL_AMOUNT_PER_STOCK`** — dollar amount to invest per slot when topping up an undersized portfolio (e.g. 5000)

Each week, for every stock you currently hold:

| Condition | Action |
| --- | --- |
| Rank ≤ `MAX_STOCKS` | **Keep** — still in the top tier |
| `MAX_STOCKS` < Rank ≤ `MAX_STOCKS + SLACK_VALUE` | **Keep** — in the buffer zone, avoid churn |
| Rank > `MAX_STOCKS + SLACK_VALUE` | **Sell** — fallen too far |
| Not in the rankings at all | **Sell** — no longer in the index |

After deciding what to sell, the rebalancer fills open slots by buying the highest-ranked stocks not already held (only stocks ranked ≤ `MAX_STOCKS` are eligible). Buy sizing depends on how the slot was opened:

| Slot type | How it opened | Sizing |
| --- | --- | --- |
| **Sell-funded** | Freed by a sell this cycle | Sell proceeds ÷ number of sells |
| **Organic** | Was already empty (portfolio undersized) | `INITIAL_AMOUNT_PER_STOCK` per slot |

Share counts are always rounded down to whole shares.

**Ongoing rebalance example** — `MAX_STOCKS=5`, `SLACK_VALUE=2` (threshold = 7):

```text
Index rankings this week          Current portfolio
──────────────────────────────    ─────────────────────────────────────────
Rank 1  AAPL   $195              AAPL  10 shares  @ $195  → held (rank 1 ≤ 5)
Rank 2  MSFT   $415              MSFT   5 shares  @ $415  → held (rank 2 ≤ 5)
Rank 3  GOOG   $171              NVDA   8 shares  @ $875  → held (rank 5 ≤ 5)
Rank 4  AMZN   $182              TSLA   5 shares  @ $250  → SELL (rank 9 > 7)
Rank 5  NVDA   $875
Rank 6  META   $490  ← not eligible (rank 6 > MAX_STOCKS=5, can't buy)
...
Rank 9  TSLA   $250

Step 1 — Sell
─────────────
TSLA:  sell 5 shares × $250 = $1,250 proceeds

Step 2 — Size the buys
───────────────────────
1 sell → 1 sell-funded slot.  GOOG (rank 3) gets the full sell proceeds.
Cash per slot: $1,250 ÷ 1 = $1,250

Open slots: MAX_STOCKS(5) − retained(3) = 2 slots → GOOG (sell-funded), AMZN (organic)
Organic slot AMZN: INITIAL_AMOUNT_PER_STOCK = $875

Step 3 — Buy
─────────────
GOOG:  floor($1,250 ÷ $171) = 7 shares   ← sell-funded
AMZN:  floor($875  ÷ $182) = 4 shares   ← organic (InitialAmountPerStock)

Final portfolio: AAPL, MSFT, NVDA, GOOG, AMZN
```

**First-run / empty portfolio example** — `MAX_STOCKS=5`, `INITIAL_AMOUNT_PER_STOCK=5000`:

```text
No positions held → all 5 slots are organic.

Buy the top-5 ranked stocks, each sized at $5,000:
  AAPL (rank 1 @ $195):  floor($5,000 ÷ $195) = 25 shares
  MSFT (rank 2 @ $415):  floor($5,000 ÷ $415) = 12 shares
  GOOG (rank 3 @ $171):  floor($5,000 ÷ $171) = 29 shares
  AMZN (rank 4 @ $182):  floor($5,000 ÷ $182) = 27 shares
  NVDA (rank 5 @ $875):  floor($5,000 ÷ $875) =  5 shares

Ensure your brokerage account holds at least MAX_STOCKS × INITIAL_AMOUNT_PER_STOCK
in cash before the first run (5 × $5,000 = $25,000 in this example).
```

> **Note:** META is rank 6, which is within the slack zone, but it is **not eligible for buying** — only stocks ranked ≤ `MAX_STOCKS` (≤ 5) can be purchased. The slack zone only protects stocks you *already hold* from being sold prematurely.

---

## Supported indices

You can run as many portfolios as you want in parallel. Each one tracks a different index with its own broker account and strategy settings.

| Index key | What it is |
| --- | --- |
| `sp500` | S&P 500 |
| `ndx` | Nasdaq-100 |
| `sp400` | S&P 400 Mid-Cap |
| `sp600` | S&P 600 Small-Cap |

---

## Setup guide

### Step 1 — Fork this repository

Click **Fork** on GitHub. You get your own copy that runs the Action on your account.

---

### Step 2 — Get your Tradier credentials

> **Important: one Tradier account per profile, strictly.**
>
> Each profile must use its own dedicated Tradier account. The rebalancer reads the positions, cash balance, and open orders of each account independently and manages them as a self-contained portfolio. If two profiles share the same account, both will see the same positions pool and cash balance, buy and sell the same tickers simultaneously, and produce unpredictable results — including doubled positions, conflicting orders, and incorrect buy sizing. **Do not reuse the same account number across multiple profiles.**

For each index profile you plan to run:

1. Sign up at [tradier.com](https://tradier.com) (or create a paper trading account at [developer.tradier.com](https://developer.tradier.com) to test first).
2. Open a **separate brokerage account** for each profile. Tradier supports multiple accounts under one login.
3. Go to your Tradier dashboard → **API Access** → generate a Bearer token for each account.
4. Note the **Account Number** of each account from the dashboard.

Fund each account with at least `MAX_STOCKS × INITIAL_AMOUNT_PER_STOCK` dollars before the first run so the rebalancer has enough cash to fill the initial portfolio.

---

### Step 3 — Create two GitHub Environments

This project uses GitHub Environments to keep live and paper-trading credentials completely separate. Each workflow reads only from its own environment.

Go to your fork → **Settings → Environments → New environment** and create two environments:

| Environment | Used by | Holds |
| --- | --- | --- |
| `PROD` | Weekly rebalance workflow | Live Tradier account credentials |
| `BACKTEST` | Paper trading backtest workflow | Sandbox Tradier account credentials |

> **Tip:** You can add protection rules to the `PROD` environment (e.g. require a reviewer before the workflow runs) under the environment settings.

---

### Step 4 — Configure the PROD environment

Inside the **PROD** environment, add the following Variables and Secrets.

#### Variables (non-sensitive, visible in logs)

| Variable | Description | Example |
| --- | --- | --- |
| `PROFILE_COUNT` | How many index portfolios to run | `2` |
| `PROFILE_N_NAME` | Human-readable label shown in logs | `SP500 Momentum` |
| `PROFILE_N_INDEX` | Index key — one of: `sp500`, `sp400`, `sp600`, `ndx` | `sp500` |
| `PROFILE_N_MAX_STOCKS` | Target number of holdings | `25` |
| `PROFILE_N_SLACK_VALUE` | Retention buffer (0 = no slack) | `5` |
| `PROFILE_N_INITIAL_AMOUNT_PER_STOCK` | Dollar amount per organic buy slot | `5000` |

Replace `N` with `1`, `2`, `3`, etc. for each profile.

#### Secrets (encrypted, never shown in logs)

| Secret | Description |
| --- | --- |
| `RANKING_API_TOKEN` | QuantMyStocks API Bearer token |
| `PROFILE_N_TRADIER_TOKEN` | Tradier Bearer token for profile N (live account) |
| `PROFILE_N_TRADIER_ACCOUNT_ID` | Tradier account number for profile N (live account) |

**Example — PROD with two profiles (each using a separate Tradier account):**

```text
# Variables
PROFILE_COUNT           = 2

PROFILE_1_NAME                     = SP500 Momentum
PROFILE_1_INDEX                    = sp500
PROFILE_1_MAX_STOCKS               = 25
PROFILE_1_SLACK_VALUE              = 5
PROFILE_1_INITIAL_AMOUNT_PER_STOCK = 5000

PROFILE_2_NAME                     = NDX Top 10
PROFILE_2_INDEX                    = ndx
PROFILE_2_MAX_STOCKS               = 10
PROFILE_2_SLACK_VALUE              = 2
PROFILE_2_INITIAL_AMOUNT_PER_STOCK = 3000

# Secrets — note: each profile has its own account number and token
RANKING_API_TOKEN             = <your QuantMyStocks API token>
PROFILE_1_TRADIER_TOKEN       = <token for SP500 account>
PROFILE_1_TRADIER_ACCOUNT_ID  = <account number for SP500 account>
PROFILE_2_TRADIER_TOKEN       = <token for NDX account — different account>
PROFILE_2_TRADIER_ACCOUNT_ID  = <account number for NDX account — different number>
```

---

### Step 4b — Configure the BACKTEST environment

Inside the **BACKTEST** environment, add the same variable and secret names but with your Tradier **sandbox** account credentials. The strategy parameters (`PROFILE_1_INDEX`, `MAX_STOCKS`, etc.) can match PROD or be different — the BACKTEST environment is fully independent.

```text
# Variables
PROFILE_1_NAME                     = SP500 Backtest
PROFILE_1_INDEX                    = sp500
PROFILE_1_MAX_STOCKS               = 25
PROFILE_1_SLACK_VALUE              = 5
PROFILE_1_INITIAL_AMOUNT_PER_STOCK = 5000

# Secrets
RANKING_API_TOKEN             = <your QuantMyStocks API token>
PROFILE_1_TRADIER_TOKEN       = <your SANDBOX account token>
PROFILE_1_TRADIER_ACCOUNT_ID  = <your SANDBOX account number>
```

The `PROFILE_1_TRADIER_TOKEN` in BACKTEST holds sandbox credentials — the same variable name, but a different value. GitHub scopes secrets to the environment, so PROD and BACKTEST are completely isolated.

---

### Step 4c — Set up email notifications (optional)

After each run the workflow prints a formatted summary to the Actions log and to the job summary page. If you also want that summary delivered to your inbox, add four SMTP secrets to your GitHub environments.

The email step is **completely optional** — if any of the four secrets are absent the step is silently skipped and everything else continues to work normally.

---

#### Getting SMTP credentials with Gmail

Gmail is the easiest option. You need to create an **App Password** — a one-time 16-character token that lets GitHub Actions send email on your behalf without exposing your real password.

> **Prerequisite:** Your Google account must have 2-Step Verification enabled. If it is not, go to [myaccount.google.com/security](https://myaccount.google.com/security) and turn it on first.

1. Go to [myaccount.google.com/apppasswords](https://myaccount.google.com/apppasswords).
2. Under **App name**, type anything (e.g. `quant-stocks`).
3. Click **Create**.
4. Google shows a 16-character password (e.g. `abcd efgh ijkl mnop`). Copy it — it is shown only once.
5. Use these values for the secrets below:

| Secret | Value |
| --- | --- |
| `SMTP_SERVER` | `smtp.gmail.com` |
| `SMTP_USERNAME` | your Gmail address (e.g. `you@gmail.com`) |
| `SMTP_PASSWORD` | the 16-character App Password from step 4 |
| `NOTIFY_EMAIL` | the address to send summaries to (can be the same address) |

> **Other email providers:** Most providers that support SMTP over port 587 with STARTTLS will work. Set `SMTP_SERVER` to your provider's outgoing SMTP host and `SMTP_USERNAME` / `SMTP_PASSWORD` to your login credentials for that service.

---

#### Adding the secrets to GitHub

Add the four secrets to **each environment** you want email notifications for:

- **PROD environment** → Settings → Environments → PROD → Secrets
- **BACKTEST environment** → Settings → Environments → BACKTEST → Secrets

You can reuse the same App Password in both environments — it is not scoped to a specific workflow.

| Secret | Description |
| --- | --- |
| `SMTP_SERVER` | SMTP host, e.g. `smtp.gmail.com` |
| `SMTP_USERNAME` | Your login address, e.g. `you@gmail.com` |
| `SMTP_PASSWORD` | App Password (not your regular account password) |
| `NOTIFY_EMAIL` | Recipient address — where summaries are delivered |

Once added, every workflow run sends an email with the full rebalance or backtest summary in the body. The subject lines are:

- **Weekly rebalance:** `[quant-stocks] Weekly Rebalance — <run number>`
- **Backtest:** `[quant-stocks] Backtest — <N> weeks (run #<run number>)`

---

### Step 5 — Rankings are fetched automatically

The rebalancer calls the QuantMyStocks leaderboard API on every run — no CSV files or manual data preparation required. It sends a POST request with the index identifier and the most recent trading day, and receives a ranked list of tickers in response. Current prices are then fetched from Tradier's quotes endpoint to calculate share counts.

| Index key | QuantMyStocks index |
| --- | --- |
| `sp500` | S&P 500 |
| `sp400` | S&P 400 Mid-Cap |
| `sp600` | S&P 600 Small-Cap |
| `ndx` | Nasdaq-100 |

Rankings reflect the most recent market close (Friday's close when the Action runs on Monday morning).

---

### Step 6 — Verify the workflow schedule

All orders use `duration=gtc` (good-till-cancelled) so an order placed before the market opens queues and fills when trading starts, rather than being rejected.

The workflow runs automatically on the following schedule (all times ET):

| Day | Time | Trigger |
| --- | --- | --- |
| Monday | 3:30 AM | 30 min before pre-market — places GTC orders ahead of trading |
| Monday | 4:05 AM | 5 min after pre-market opens (4:00 AM) |
| Monday | 9:35 AM | 5 min after regular market opens (9:30 AM) |
| Tue – Fri | 4:05 AM | Pre-market open — picks up any unfilled GTC orders or new signals |
| Tue – Fri | 9:35 AM | Regular market open |

The idempotency guard (open-orders check) ensures that if an order was already placed in an earlier run and is still pending, it will be skipped on the next run — no duplicate orders.

You can also trigger it manually at any time from **Actions → Weekly Portfolio Rebalance → Run workflow**.

---

## Testing before going live

Use the **Paper Trading Backtest** workflow to validate your strategy settings against your Tradier sandbox account before enabling live trading. See the [Paper-trading backtest](#paper-trading-backtest) section for setup instructions.

The rebalancer will never place the same order twice in one run. If a sell or buy for a ticker is already pending at Tradier (from a previous run or a manual order), it is automatically skipped.

---

## Paper-trading backtest

You can replay the last N weeks of the strategy against your Tradier **sandbox** (paper trading) account. The backtest fetches historical leaderboard rankings from QuantMyStocks for each past Sunday, then executes real paper orders at current live prices.

### How it works

1. All existing positions in the paper account are liquidated for a clean start.
2. For each of the past `BACKTEST_WEEKS` Sundays (oldest → newest):
   - Fetch the QuantMyStocks leaderboard as of that Sunday.
   - Fetch current live prices from Tradier for all ranked tickers.
   - Run the rebalance strategy and execute paper sells, then paper buys.
   - Wait `BACKTEST_DELAY_SECONDS` before the next week (default: 120 s).
3. Print a detailed per-week summary to stdout.

> **Note:** Prices used for buy sizing are today's live prices, not historical prices. Rankings are historical (the leaderboard snapshot for that Sunday), but execution happens at whatever the market price is right now. This is consistent with how the weekly runner works — it uses the most recent Sunday's ranks together with the current opening price.

---

### Running from GitHub Actions

The easiest way to run a backtest is directly from your fork. Go to **Actions → Paper Trading Backtest → Run workflow**, fill in the number of weeks and an optional delay, then click **Run workflow**.

The workflow reads all configuration from the **BACKTEST** GitHub Environment (set up in Step 4b above). It uses your `PROFILE_1_*` variables for strategy settings and `PROFILE_1_TRADIER_TOKEN` / `PROFILE_1_TRADIER_ACCOUNT_ID` for your sandbox account credentials. The binary automatically routes to `sandbox.tradier.com` in backtest mode — it never touches your live account.

---

### Running a backtest locally

```bash
BACKTEST_MODE=true \
  BACKTEST_WEEKS=12 \
  BACKTEST_DELAY_SECONDS=120 \
  PROFILE_COUNT=1 \
  PROFILE_1_NAME="SP500 Backtest" \
  PROFILE_1_INDEX=sp500 \
  PROFILE_1_MAX_STOCKS=25 \
  PROFILE_1_SLACK_VALUE=5 \
  PROFILE_1_INITIAL_AMOUNT_PER_STOCK=5000 \
  PROFILE_1_TRADIER_TOKEN="<your sandbox token>" \
  PROFILE_1_TRADIER_ACCOUNT_ID="<your sandbox account ID>" \
  RANKING_API_TOKEN="<your QuantMyStocks token>" \
  go run ./cmd/rebalancer
```

### Backtest environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `BACKTEST_MODE` | Yes | Set to `true` to enable backtest mode |
| `BACKTEST_WEEKS` | Yes | Number of past weeks to simulate (e.g. `12`) |
| `BACKTEST_DELAY_SECONDS` | No | Pause between weekly runs in seconds (default: `120`) |

All normal `PROFILE_1_*` variables and `RANKING_API_TOKEN` are also required. The binary automatically routes to `sandbox.tradier.com` in backtest mode — provide your Tradier sandbox account credentials.

---

## Troubleshooting

**Workflow fails with "GitHub Secret RANKING_API_TOKEN is not set"**
→ Add your QuantMyStocks API token as a Secret named `RANKING_API_TOKEN`. Follow Step 4 above.

**Workflow fails with "GitHub Secret PROFILE_1_TRADIER_TOKEN is not set"**
→ A required Secret was not added. Follow Step 4 above.

**Workflow fails with "GitHub Variable PROFILE_COUNT is not set"**
→ Go to Variables (not Secrets) and add `PROFILE_COUNT`.

**Workflow fails with "GitHub Variable PROFILE_1_MAX_STOCKS must be a positive integer"**
→ The value was set as a word (e.g. `twenty-five`). It must be a plain number (e.g. `25`).

**Workflow fails with "GitHub Variable PROFILE_1_INITIAL_AMOUNT_PER_STOCK is not set"**
→ Go to Variables and add `PROFILE_N_INITIAL_AMOUNT_PER_STOCK` with the dollar amount to invest per position on initial fill (e.g. `5000`). This is required for all profiles.

**Orders are placed but shares = 0 in the logs**
→ The allocated amount per slot is less than the price of one share.

- For **sell-funded** slots: sell proceeds ÷ number of sells is too small. Either the sold position had low value or you are selling many stocks at once with little proceeds per slot.
- For **organic** slots: `PROFILE_N_INITIAL_AMOUNT_PER_STOCK` is lower than the stock price. Increase this value or choose a lower-priced index to target.

**Buy orders fail immediately after sells with "insufficient buying power" or similar**
→ The account does not have enough settled cash to fund the buys. Ensure the account is funded with at least `MAX_STOCKS × INITIAL_AMOUNT_PER_STOCK` before the run, or wait until the sell proceeds settle and trigger the workflow again.

**Two profiles are buying or selling the same ticker, or cash sizing looks wrong**
→ You are likely using the same Tradier account for multiple profiles. Each profile reads positions, cash, and open orders from its own account independently. When two profiles share an account they see the same cash balance and the same position list — both profiles try to manage the same tickers simultaneously, producing doubled positions, conflicting orders, and incorrect buy sizing. Assign a separate Tradier account (and token) to each profile.

**Email step is skipped even though secrets are set**
→ Verify all four secrets (`SMTP_SERVER`, `SMTP_USERNAME`, `SMTP_PASSWORD`, `NOTIFY_EMAIL`) are present in the correct environment (PROD or BACKTEST). A missing secret makes the whole group evaluate to empty and the step is skipped.

**Email step fails with "authentication failed"**
→ Your `SMTP_PASSWORD` is your regular account password. Gmail requires an App Password — follow Step 4c above to generate one. Regular passwords are rejected when 2-Step Verification is enabled.

**Email step fails with "connection refused" or timeout**
→ Check that `SMTP_SERVER` is correct (e.g. `smtp.gmail.com`) and that port 587 is not blocked. Some corporate SMTP servers require port 465 — consult your provider's documentation.

**A stock stays in the portfolio even though its rank dropped**
→ This is expected if its rank is still within the slack zone (`rank ≤ MAX_STOCKS + SLACK_VALUE`). Reduce `SLACK_VALUE` if you want faster turnover.

---

## Adding more than 5 profiles

The workflow ships with slots for 5 profiles. If you need more, open `.github/workflows/weekly-rebalance.yml` in your fork and copy the `PROFILE_5_*` block, incrementing the number to `6`, `7`, etc. Then set the matching Variables and Secrets in GitHub.

---

## Running locally

```bash
# Clone your fork
git clone https://github.com/your-username/quant-stocks
cd quant-stocks

# Run with mock broker and mock rankings (no credentials needed)
PROFILE_COUNT=1 \
  PROFILE_1_NAME="Test" \
  PROFILE_1_INDEX=sp500 \
  PROFILE_1_MAX_STOCKS=5 \
  PROFILE_1_SLACK_VALUE=2 \
  PROFILE_1_INITIAL_AMOUNT_PER_STOCK=5000 \
  PROFILE_1_BROKER_TYPE=mock \
  RANKING_MODE=mock \
  go run ./cmd/rebalancer

# Run against the live QuantMyStocks API with mock broker
PROFILE_COUNT=1 \
  PROFILE_1_NAME="SP500" \
  PROFILE_1_INDEX=sp500 \
  PROFILE_1_MAX_STOCKS=25 \
  PROFILE_1_SLACK_VALUE=5 \
  PROFILE_1_INITIAL_AMOUNT_PER_STOCK=5000 \
  PROFILE_1_BROKER_TYPE=mock \
  RANKING_API_TOKEN="<your token>" \
  go run ./cmd/rebalancer

# Run domain unit tests
go test ./internal/domain/... -v
```

---

## Project layout

```text
quant-stocks/
├── cmd/rebalancer/main.go              # Entry point — reads env vars, runs profiles
├── internal/
│   ├── config/config.go                # Loads profiles from environment variables
│   ├── domain/
│   │   ├── entity.go                   # Core types (Position, Rank, Order, …)
│   │   ├── rebalancer.go               # Pure strategy logic — no I/O
│   │   └── rebalancer_test.go          # Unit tests for the strategy
│   ├── port/
│   │   ├── broker.go                   # Broker interface
│   │   └── ranking.go                  # RankingProvider interface
│   ├── service/rebalance_service.go    # Orchestration (fetch → compute → execute)
│   └── adapter/
│       ├── broker/
│       │   ├── tradier_broker.go       # Tradier REST API adapter
│       │   └── mock_broker.go          # In-memory mock for testing
│       └── ranking/
│           ├── quantmystocks.go        # QuantMyStocks leaderboard API adapter
│           └── mock_ranking.go         # Hardcoded mock for local testing
└── .github/workflows/weekly-rebalance.yml
```
