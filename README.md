# quant-stocks

A weekly stock portfolio rebalancer that runs automatically as a GitHub Action every Monday morning. It connects to your Tradier brokerage account, reads a momentum rankings file you provide, and executes the minimum set of trades needed to keep your portfolio aligned with the strategy.

You configure everything through GitHub Secrets and Variables — no code changes required.

---

## How the strategy works

The rebalancer uses a **slack-based momentum strategy**. You configure two numbers per portfolio:

- **`MAX_STOCKS`** — target number of holdings (e.g. 25)
- **`SLACK_VALUE`** — a buffer that prevents unnecessary selling (e.g. 5)

Each week, for every stock you currently hold:

| Condition | Action |
|---|---|
| Rank ≤ `MAX_STOCKS` | **Keep** — still in the top tier |
| `MAX_STOCKS` < Rank ≤ `MAX_STOCKS + SLACK_VALUE` | **Keep** — in the buffer zone, avoid churn |
| Rank > `MAX_STOCKS + SLACK_VALUE` | **Sell** — fallen too far |
| Not in the rankings at all | **Sell** — no longer in the index |

After selling, the proceeds are split equally and used to **buy the highest-ranked stocks not already held**, until the portfolio is back to `MAX_STOCKS` positions.

**Example** — `MAX_STOCKS=5`, `SLACK_VALUE=2` (threshold = 7):

```
Held:  AAPL(rank 1), MSFT(rank 2), TSLA(rank 9), NVDA(rank 6)
───────────────────────────────────────────────────────────────
AAPL  rank 1  ≤ 5        → keep
MSFT  rank 2  ≤ 5        → keep
NVDA  rank 6  ≤ 7        → keep (slack zone)
TSLA  rank 9  > 7        → SELL

Proceeds split across 2 open slots → BUY GOOG (rank 3), AMZN (rank 4)

Final: AAPL, MSFT, NVDA, GOOG, AMZN
```

---

## Supported indices

You can run as many portfolios as you want in parallel. Each one tracks a different index with its own broker account and strategy settings.

| Index key | What it is |
|---|---|
| `sp500` | S&P 500 |
| `ndx` | Nasdaq-100 |
| `sp400` | S&P 400 Mid-Cap |
| `sp600` | S&P 600 Small-Cap |
| any string | Your own custom ranking list |

---

## Setup guide

### Step 1 — Fork this repository

Click **Fork** on GitHub. You get your own copy that runs the Action on your account.

---

### Step 2 — Get your Tradier credentials

1. Sign up at [tradier.com](https://tradier.com) (or use their paper trading sandbox at [developer.tradier.com](https://developer.tradier.com) to test first).
2. Go to your Tradier dashboard → **API Access** → generate a Bearer token.
3. Note your **Account Number** from the dashboard.

Repeat for each index portfolio you want to run (each portfolio needs its own Tradier account and token).

---

### Step 3 — Set GitHub Variables

Go to your fork → **Settings → Secrets and Variables → Actions → Variables → New repository variable**.

Add one variable at a time using the names below. Start with `PROFILE_COUNT`, then fill in the profile-specific variables for each index you want to track.

#### Required for all setups

| Variable | Description | Example |
|---|---|---|
| `PROFILE_COUNT` | How many index portfolios to run | `2` |

#### Required per profile (repeat for each profile number)

Replace `N` with `1`, `2`, `3`, etc.

| Variable | Description | Example |
|---|---|---|
| `PROFILE_N_NAME` | Human-readable label shown in logs | `SP500 Momentum` |
| `PROFILE_N_INDEX` | Index key — must match your rankings file | `sp500` |
| `PROFILE_N_MAX_STOCKS` | Target number of holdings | `25` |
| `PROFILE_N_SLACK_VALUE` | Retention buffer (0 = no slack) | `5` |
| `PROFILE_N_TRADIER_SANDBOX` | `true` for paper trading, `false` for live | `false` |

**Example — two profiles:**

```
PROFILE_COUNT           = 2

PROFILE_1_NAME          = SP500 Momentum
PROFILE_1_INDEX         = sp500
PROFILE_1_MAX_STOCKS    = 25
PROFILE_1_SLACK_VALUE   = 5
PROFILE_1_TRADIER_SANDBOX  = false

PROFILE_2_NAME          = NDX Top 10
PROFILE_2_INDEX         = ndx
PROFILE_2_MAX_STOCKS    = 10
PROFILE_2_SLACK_VALUE   = 2
PROFILE_2_TRADIER_SANDBOX  = false
```

---

### Step 4 — Set GitHub Secrets

Go to **Settings → Secrets and Variables → Actions → Secrets → New repository secret**.

These are encrypted and never shown in logs.

#### Required — one shared secret for rankings

| Secret | Description |
|---|---|
| `RANKING_API_TOKEN` | Your QuantMyStocks API Bearer token |

Get your token from your QuantMyStocks account dashboard.

#### Required per profile — Tradier credentials

| Secret | Description |
|---|---|
| `PROFILE_N_TRADIER_TOKEN` | Tradier Bearer token for profile N |
| `PROFILE_N_TRADIER_ACCOUNT_ID` | Tradier account number for profile N |

**Example — two profiles:**

```
RANKING_API_TOKEN             = <your QuantMyStocks API token>

PROFILE_1_TRADIER_TOKEN       = <your SP500 account token>
PROFILE_1_TRADIER_ACCOUNT_ID  = <your SP500 account number>

PROFILE_2_TRADIER_TOKEN       = <your NDX account token>
PROFILE_2_TRADIER_ACCOUNT_ID  = <your NDX account number>
```

---

### Step 5 — Rankings are automatic

No extra setup required. The rebalancer fetches momentum rankings automatically from the [QuantMyStocks](https://quantmystocks.com) leaderboard API every time it runs.

Just set `PROFILE_N_INDEX` to one of the supported values:

| Index key | Index |
|---|---|
| `sp500` | S&P 500 |
| `sp400` | S&P 400 Mid-Cap |
| `sp600` | S&P 600 Small-Cap |
| `ndx` | Nasdaq-100 |

Rankings reflect the most recent market close (Friday's close when the Action runs on Monday morning).

---

### Step 6 — Verify the workflow schedule

The Action runs automatically every **Monday at 14:35 UTC (10:35 AM ET)**, which is 5 minutes after the US market opens.

You can trigger it manually at any time from **Actions → Weekly Portfolio Rebalance → Run workflow**. Manual runs use the same settings — set `PROFILE_N_TRADIER_SANDBOX=true` to run safely against Tradier's paper trading environment first.

---

## Testing before going live

We recommend this sequence:

1. Set `PROFILE_N_TRADIER_SANDBOX=true` for all profiles.
2. Trigger the workflow manually from the Actions tab.
3. Verify the log output — check that the right stocks are being bought and sold.
4. Once satisfied, set `PROFILE_N_TRADIER_SANDBOX=false` to switch to your live account.

The rebalancer will never place the same order twice in one run. If a sell or buy for a ticker is already pending at Tradier (from a previous run or a manual order), it is automatically skipped.

---

## Troubleshooting

**Workflow fails with "GitHub Variable PROFILE_1_TRADIER_TOKEN is not set"**
→ A required Secret was not added. Follow Step 4 above.

**Workflow fails with "GitHub Variable PROFILE_COUNT is not set"**
→ Go to Variables (not Secrets) and add `PROFILE_COUNT`.

**Workflow fails with "GitHub Variable PROFILE_1_MAX_STOCKS must be a positive integer"**
→ The value was set as a word (e.g. `twenty-five`). It must be a plain number (e.g. `25`).

**Orders are placed but shares = 0 in the logs**
→ The cash available after sells is less than the price of one share for that stock. Either increase your initial cash balance in Tradier or reduce `MAX_STOCKS` so each position gets a larger allocation.

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
  PROFILE_1_BROKER_TYPE=mock \
  RANKING_MODE=mock \
  go run ./cmd/rebalancer

# Run against the live QuantMyStocks API with mock broker
PROFILE_COUNT=1 \
  PROFILE_1_NAME="SP500" \
  PROFILE_1_INDEX=sp500 \
  PROFILE_1_MAX_STOCKS=25 \
  PROFILE_1_SLACK_VALUE=5 \
  PROFILE_1_BROKER_TYPE=mock \
  go run ./cmd/rebalancer

# Run domain unit tests
go test ./internal/domain/... -v
```

---

## Project layout

```
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
│           └── mock_ranking.go         # Hardcoded mock for testing
└── .github/workflows/weekly-rebalance.yml
```
