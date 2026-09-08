# Robinhood Trading MCP integration plan

## Status and decisions

Robinhood support is **planned, not implemented**. The production binary currently supports Tradier and the in-memory mock broker only. Do not set `PROFILE_N_BROKER_TYPE=robinhood` until the adapter, authentication, reconciliation, and tests described here are complete.

The agreed design is:

- Call Robinhood's Trading MCP directly from Go. An LLM is not part of the trading path.
- Use Robinhood's dedicated Agentic brokerage account, not the primary Robinhood account.
- Submit equity orders as **market + day** orders.
- Allow pre-market runs to queue orders for the next regular trading session.
- Run the bot multiple times per day. A rejected or canceled buy may be retried by a later run after sell proceeds become available.
- Do not add an application database for trade state. Reconstruct the current rebalance cycle from Robinhood positions, buying power, and order history.
- Persist OAuth credentials in the GitHub `PROD` environment and write rotated credentials back to the environment secret immediately.
- Keep Tradier as a separate adapter and preserve its current behavior.

Robinhood's official MCP endpoint is:

```text
https://agent.robinhood.com/mcp/trading
```

## Important account constraints

Robinhood Trading MCP trades through a separate Agentic account. An existing personal Robinhood login is the starting point for enrollment, subject to Robinhood's eligibility and onboarding, but the bot cannot use the existing primary brokerage account as if it were an API account.

The Agentic account supports limited margin. This makes unsettled proceeds from a completed sale available for another purchase, but it does not provide margin borrowing or leverage. A queued sell does not fund a queued buy: the sell must execute before its proceeds increase buying power.

Each bot profile must control an isolated brokerage account. Do not point two profiles at the same Agentic account, and do not place manual trades in an account managed by the bot. Stateless reconciliation relies on the account's order history being attributable to one profile.

## Proposed architecture

```text
RebalanceService
    |
    +-- Broker interface
          |
          +-- TradierBroker ----------------> Tradier REST API
          |
          +-- RobinhoodBroker
                 |
                 +-- MCP client + OAuth ----> Robinhood Trading MCP
```

`RobinhoodBroker` should translate typed domain operations into MCP tool calls. The strategy and ranking provider should not know that MCP is involved.

Use the official Model Context Protocol Go SDK with Streamable HTTP and OAuth support. Pin a tested SDK release. The repository currently targets Go 1.22, so confirm SDK compatibility in the dependency spike and upgrade the Go toolchain deliberately if the selected release requires it.

## Robinhood tool mapping

Robinhood publishes these relevant equity tools. Inspect their live schemas during implementation; do not hard-code parameters based only on names.

| Bot operation | Robinhood MCP tool | Notes |
| --- | --- | --- |
| Discover and validate account | `get_accounts` | Select only the configured Agentic account and fail closed if it is absent or ambiguous. |
| Read portfolio and buying power | `get_portfolio` | Use broker-reported buying power as the final affordability check. |
| Read current holdings | `get_equity_positions` | Normalize symbols and quantities into domain positions. |
| Fetch prices | `get_equity_quotes` | The tool accepts up to 20 symbols; request only required symbols and chunk larger batches. |
| Reconstruct order state | `get_equity_orders` | Read current and historical orders for the active cycle, including terminal and partial states. |
| Check whether a symbol can trade | `get_equity_tradability` | Fail or skip before constructing an order for an unsupported symbol. |
| Validate an order | `review_equity_order` | Mandatory preflight before placement; validate side, quantity, order type, duration, and estimated cost. |
| Submit an order | `place_equity_order` | Place only the exact reviewed market + day order. |
| Cancel an order | `cancel_equity_order` | Administrative/recovery path; normal reconciliation should not cancel healthy pending orders. |

## Market + day behavior

Market orders submitted outside regular market hours are queued for the next regular session. They do not execute during extended hours. Robinhood may reserve approximately 5% more buying power for a queued market buy to protect against an opening-price move, and the order can still be canceled at the open if buying power is insufficient.

This means the current schedule can remain multi-run, with these semantics:

1. A pre-market run may queue sells and buys.
2. Queued buys may fail because the corresponding sells have not executed yet or because Robinhood's buying-power reserve is larger than the estimate.
3. After sells execute, limited margin makes their unsettled proceeds available.
4. A later run sees the filled sells and rejected/canceled buys, recalculates the remaining budget, and retries eligible buys.
5. Day orders that do not fill expire at the end of their eligible session; they are not GTC orders.

Never assume that submitting sells first in one process means they have filled before the buy loop begins. Submission order and fill order are different.

If the account already has sufficient buying power, Robinhood may approve both a queued sell and its replacement buy. If it does not, the bot should avoid knowingly unaffordable placement when the review exposes that condition. A buy that is nevertheless rejected or canceled remains eligible after a later run observes filled sell proceeds or increased buying power.

## Stateless reconciliation

### Source of truth

Robinhood becomes the durable trade-state store. Every run must start from fresh broker data:

- Agentic account identity
- current positions
- current buying power
- all equity orders relevant to the active weekly ranking cycle
- current quotes for held and candidate symbols
- the ranking snapshot for the cycle

The active cycle is identified by the portfolio profile, Agentic account, index, and ranking date. Compute the ranking date in `America/New_York` so a UTC boundary cannot move a Sunday/Monday run into the wrong cycle.

Reconstruct the opening position for each symbol by reversing fills from the active cycle:

```text
opening quantity = current quantity + cycle sell fills - cycle buy fills
```

This reconstructed opening portfolio determines which slots were organic and which were created by cycle sells. It also makes the calculation deterministic after a sold position disappears from the current-position response. Corporate actions or manual trades can invalidate the reconstruction, which is another reason the Agentic account must be dedicated to one bot profile.

If `place_equity_order` exposes a client-order-id field, populate it with a deterministic identifier containing the profile, ranking date, symbol, side, and logical slot. If it does not, reconcile with the Robinhood order id, account, symbol, side, submission time, requested quantity, and cycle window. This fallback is safe only when the Agentic account is dedicated to the bot.

### Order-state rules

Normalize Robinhood statuses into these domain states:

| State | Reconciliation action |
| --- | --- |
| Pending/open/queued | Count the order as committed and do not submit a duplicate. |
| Partially filled and active | Count the filled portion and the remaining open portion; do not replace it. |
| Filled | Count actual filled quantity and average fill price as completed execution. |
| Rejected/canceled/expired | Do not count an unfilled remainder as committed; it is eligible for a later retry. |
| Unknown | Fail closed for that symbol and alert; do not guess. |

Do not deduplicate with only `symbol + side`. That is sufficient for the current open-order guard, but it cannot distinguish an old cycle, a completed order, a retry, or a partially filled order.

### Per-run algorithm

For each profile, execute this sequence:

1. Connect and authenticate to the Robinhood MCP endpoint.
2. Call `get_accounts`; require the configured Agentic account.
3. Read positions, portfolio/buying power, and equity order history.
4. Fetch the current ranking snapshot.
5. Filter and normalize broker orders for the active ranking cycle, reconstruct the opening portfolio, and derive the deterministic desired portfolio.
6. Reconcile sells:
   - If a required sell is pending, skip it.
   - If it partially filled and remains active, wait.
   - If it filled, record actual proceeds as `filled quantity × average fill price`.
   - If it failed and the position remains, review and submit a new market + day sell.
7. Reconcile replacement buys:
   - Derive replacement slots from sells belonging to this cycle, including sells that filled on an earlier run.
   - Count filled replacement buys at actual cost and active buy remainders at their broker review/reservation estimate.
   - Compute `remaining realized replacement budget = actual filled sell proceeds - committed replacement buy value`, clamped to zero.
   - Treat the marked value of a still-pending sell as a planning estimate, not as available cash. Submit an early replacement buy only when Robinhood's review and current buying power allow it.
   - Allocate only the non-negative remaining budget across deterministic, unfilled replacement candidates.
8. Reconcile organic buys separately. A slot that was empty before this cycle uses `INITIAL_AMOUNT_PER_STOCK`; a slot created by a cycle sell uses only the replacement budget.
9. Limit every buy by current broker buying power and Robinhood's review estimate/reserve. An unaffordable buy is skipped for this run, not converted into margin borrowing. A terminally failed buy becomes retryable after a funding condition changes.
10. For each new order, check tradability, fetch a current quote, round to the strategy's whole-share quantity, call `review_equity_order`, validate the returned review, and then call `place_equity_order`.
11. Refresh broker order state after submissions and emit a cycle summary. Return a non-zero result or alert when an order has an unknown outcome.

### Why completed sell history is required

Suppose a cycle sells TSLA for $2,500 and the intended replacement buy fails. On the next run, TSLA is no longer a position. If the bot looks only at current positions, it sees an ordinary empty slot and incorrectly assigns `INITIAL_AMOUNT_PER_STOCK`—for example, $5,000.

The filled TSLA order in `get_equity_orders` proves that the slot was opened by a sell in this cycle. The retry therefore retains the $2,500 replacement budget, adjusted for actual fills and any already committed replacement buy.

### Ambiguous placement outcomes

Do not automatically repeat `place_equity_order` after a timeout or connection loss. First query `get_equity_orders` and match the attempted order. If there is one matching order, adopt its Robinhood id and status. If no deterministic client id is supported and the result remains ambiguous, skip that symbol and alert rather than risk a duplicate.

Broker reconciliation provides practical idempotency, but it cannot prove exactly-once execution across every network failure unless Robinhood accepts an idempotency/client-order key. That limitation must remain visible in logs and alerts.

## Authentication and unattended execution

Eliminating a trade-state database does not eliminate OAuth state. The MCP client still needs durable, encrypted storage for access and refresh credentials.

### Selected design: GitHub Environment Secret with immediate write-back

Store one complete OAuth state bundle per Robinhood profile in the protected GitHub `PROD` environment. The bundle must contain every value the selected MCP SDK needs to restart without interactive authorization, including client registration data, access token, refresh token, expiry, and granted scopes.

Use these `PROD` environment secrets:

```text
PROFILE_N_ROBINHOOD_OAUTH_STATE
ROBINHOOD_SECRET_WRITER_PRIVATE_KEY
```

Use these non-secret environment variables:

```text
ROBINHOOD_SECRET_WRITER_APP_ID
ROBINHOOD_SECRET_WRITER_INSTALLATION_ID
```

The GitHub App must be installed only on this repository and have only `Metadata: read` and repository `Secrets: write`, which are needed to replace the `PROD` environment secret. Generate a short-lived installation token during the job. Do not rely on the normal workflow `GITHUB_TOKEN` to modify Actions secrets, and do not give the trading process the GitHub App private key.

The workflow must process OAuth state in this order:

1. Materialize `PROFILE_N_ROBINHOOD_OAUTH_STATE` into a profile-specific file below `RUNNER_TEMP` with mode `0600`.
2. Start the MCP client using that file as its token store.
3. When the OAuth library receives any new or refreshed token state, atomically replace the temporary file.
4. In the token-store callback, synchronously replace the corresponding `PROD` environment secret through the GitHub Actions Secrets API. Do not defer this to a post-job cleanup step.
5. Confirm that GitHub accepted the update before allowing any order-review or order-placement call to continue.
6. If write-back fails, fail closed: make no trades, send an alert, and leave the run unsuccessful.
7. Delete the temporary credential file at the end of the job. Never upload it as an artifact or cache entry.

The current job value does not change when the secret is replaced; the update supplies the next scheduled run. A fresh run must be tested to prove that it can load the stored bundle and refresh without interactive input.

### Concurrency and crash recovery

Only one job may use a Robinhood OAuth bundle at a time. Add workflow-level serialization:

```yaml
concurrency:
  group: robinhood-agentic-prod
  cancel-in-progress: false
```

Immediate synchronous write-back minimizes the token-rotation window, but cannot remove it completely. If the runner dies after Robinhood rotates a refresh token and before GitHub accepts the replacement secret, the stored refresh token may be invalid. The recovery path is to run the local desktop bootstrap again and replace the environment secret. Never fall back to a different Robinhood account or continue trading with partially loaded credentials.

### Initial desktop bootstrap

Robinhood requires the initial Agentic authorization on a desktop. Add a local command that uses the same Go MCP client and token-store format as production, for example:

```text
go run ./cmd/rebalancer robinhood-auth --profile 1 --output <protected-temp-file>
```

After authorization succeeds, upload the file directly to `PROFILE_1_ROBINHOOD_OAUTH_STATE` with `gh secret set --env PROD`, then delete the local file. Do not copy an internal token file from Codex or another MCP client; the bot must bootstrap and persist its own client registration and OAuth grant.

### Security requirements

- Use a private repository and restrict the `PROD` environment to the production branch.
- Do not expose production secrets to pull-request or fork-triggered workflows.
- Pin third-party GitHub Actions to reviewed commit SHAs.
- Give the normal `GITHUB_TOKEN` only the permissions required by the workflow, normally `contents: read`.
- Keep the GitHub App installation limited to this repository and its secret-writing permission.
- Never commit OAuth data, print it, transform it into logs, or store it in an artifact or Actions cache. GitHub redaction is a backup, not the primary control.
- Keep a separate OAuth bundle for each Agentic account/profile.
- Mask sensitive values defensively, while assuming transformed values may not be automatically redacted.

GitHub Secrets are therefore authentication persistence, not trade-state persistence. Positions and order history remain broker-owned state.

## Configuration design

Add broker-neutral configuration while retaining existing Tradier variables:

```text
PROFILE_N_BROKER_TYPE=robinhood
PROFILE_N_ROBINHOOD_ACCOUNT_ID=<dedicated Agentic account id>
PROFILE_N_ROBINHOOD_MCP_URL=https://agent.robinhood.com/mcp/trading
PROFILE_N_ROBINHOOD_OAUTH_STATE_FILE=<protected runtime file populated by the workflow>
PROFILE_N_ROBINHOOD_LIVE_TRADING=false
```

Requirements:

- `ROBINHOOD_MCP_URL` should default to the official endpoint but remain injectable for tests.
- `ROBINHOOD_ACCOUNT_ID` is required and must match exactly one Agentic account.
- `ROBINHOOD_OAUTH_STATE_FILE` must point below `RUNNER_TEMP` in GitHub Actions and must never be a repository path.
- `ROBINHOOD_LIVE_TRADING` must default to `false`. Read-only and shadow modes should work without it.
- Validate that no two profiles use the same broker/account pair.
- Never accept primary-account selection as an implicit fallback.

## Code-change plan

### 1. Expand broker domain data

Update `internal/domain/entity.go` with a broker-order model containing at least:

- broker order id and optional client order id
- account id, symbol, side, type, and duration
- requested, filled, and remaining quantity
- average fill price
- normalized status
- creation and update timestamps
- logical cycle association when it can be derived

Keep `domain.Order` as the strategy instruction, but add explicit `DurationDay` and enough execution metadata to distinguish reviewed, submitted, pending, partial, filled, and terminal-failure outcomes.

### 2. Evolve the broker port

Update `internal/port/broker.go` so reconciliation can query order history, not only open orders. Prefer capabilities such as:

```go
GetOrders(ctx context.Context, query OrderQuery) ([]domain.BrokerOrder, error)
ReviewOrder(ctx context.Context, order domain.Order) (domain.OrderReview, error)
PlaceOrder(ctx context.Context, review domain.OrderReview) (domain.BrokerOrder, error)
```

Tradier can implement review locally. The mock should support scripted status transitions for deterministic tests.

### 3. Add the MCP adapter

Add an `internal/adapter/broker/robinhood` package containing:

- MCP Streamable HTTP session management
- OAuth credential-store abstraction with synchronous GitHub-secret write-back callback
- live tool discovery/schema validation
- typed request/response translation
- account safety checks
- quote batching in groups of at most 20 symbols
- Robinhood-status normalization
- mandatory review-before-place logic
- redacted structured logging

Transport, OAuth, and Robinhood tool translation should be independently testable. Do not pass untyped MCP payload maps into the service layer.

### 4. Add a reconciler before strategy execution

Refactor `internal/service/rebalance_service.go` so it constructs a `CycleSnapshot` before producing new orders. The snapshot contains positions plus filled, pending, failed, and partial orders from the active cycle. Feed this into a deterministic reconciliation function that returns only missing work.

Keep the calculation pure where possible. The service should perform I/O; domain code should decide whether each logical order is complete, pending, retryable, or blocked.

### 5. Wire broker selection and configuration

Update `internal/config/config.go` to validate `robinhood` profiles and `cmd/rebalancer/main.go` to construct `RobinhoodBroker`. Tradier and mock selection must continue to work unchanged.

### 6. Optimize quote requests

The current service requests quotes for every ranked symbol. Robinhood quotes should be requested only for held symbols, the `MAX_STOCKS + SLACK_VALUE` decision window, and immediate buy candidates, chunked to the MCP tool limit.

### 7. Keep backtesting separate

The existing backtest sends orders to the Tradier sandbox using historical rankings and current prices. Keep it on Tradier/mock unless Robinhood documents a separate paper environment. Do not point the backtest workflow at the live Agentic account.

### 8. Update scheduling and reporting

Retain multiple daily attempts, but describe Robinhood attempts as day-order reconciliation rather than GTC pickup. Use an explicit `America/New_York` workflow timezone where supported. Reports should separate:

- sells submitted, pending, partially filled, filled, or failed
- buys submitted, pending, partially filled, filled, skipped for buying power, or failed
- actual sell proceeds, committed buy value, and remaining replacement budget
- ambiguous outcomes requiring review

## Tests and acceptance criteria

Implementation is ready for live rollout only when these cases pass:

- MCP authentication survives a new unattended process and token refresh.
- A rotated OAuth bundle is written to the correct `PROD` environment secret before trading continues.
- A failed or timed-out OAuth write-back prevents order review and placement.
- Concurrent scheduled runs cannot use the same OAuth state.
- OAuth values do not appear in logs, artifacts, caches, or the repository.
- A configured primary or ambiguous account is rejected.
- Two profiles cannot select the same account.
- Pre-market market + day orders are represented as pending and are not duplicated.
- A filled sell followed by a failed buy is retried on a later process with the original sell-funded budget, not `INITIAL_AMOUNT_PER_STOCK`.
- Rejected, canceled, and expired buys are retryable; pending buys are not duplicated.
- Partial fills reserve both the fill and active remainder correctly.
- Actual average fill prices, not pre-trade quote estimates, determine realized sell proceeds.
- A placement timeout followed by a discoverable broker order does not create a duplicate.
- Unknown statuses and ambiguous placement outcomes fail closed.
- Quote lists larger than 20 symbols are chunked.
- The review payload exactly matches the placed order.
- Insufficient buying power skips the buy and leaves it eligible for a later run.
- Tradier and mock tests remain green.

## Rollout

1. **Connectivity:** authenticate, discover tools, validate the Agentic account, and make read-only calls.
2. **Shadow:** run the full strategy and reconciliation path, but log proposed reviews without placing orders.
3. **Review-only:** call `review_equity_order` and compare Robinhood's result with the local instruction.
4. **Canary:** enable live trading in a dedicated, minimally funded Agentic account with one profile and one position.
5. **Scheduled live:** enable the multi-run schedule and alert on rejected, canceled, expired, unknown, or ambiguous outcomes.
6. **Tradier parity:** only consider Robinhood a supported replacement after at least one complete sell/retry/buy cycle and all acceptance tests pass.

## Official references

- [Robinhood Agentic Trading overview](https://robinhood.com/us/en/support/articles/agentic-trading-overview/)
- [Robinhood Trading MCP tools and account capabilities](https://robinhood.com/us/en/support/articles/trading-with-your-agent/)
- [Robinhood market-order behavior](https://robinhood.com/us/en/support/articles/market-order-update/)
- [Robinhood order types and time in force](https://robinhood.com/us/en/support/articles/order-types/)
- [Official MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)
- [MCP authorization specification](https://modelcontextprotocol.io/specification/2025-06-18/basic/authorization)
- [GitHub Actions Secrets API](https://docs.github.com/en/rest/actions/secrets)
- [GitHub secure-use guidance](https://docs.github.com/en/actions/reference/security/secure-use)
