# sds_payment

Payment microservice in Go: turns a POS bill into a 6-digit code, and lets a
customer pay it from their wallet. Same hexagonal layout as `sds_wallet`.

```
Cashier ─▶ pos-service ──POST /internal/bills──▶ payment-service ──GET /wallets/{id}──▶ wallet-service
           (shows code)  ◀── code ──────────────
           (polls)       ──GET /internal/bills/{id}─▶

Customer ─▶ gateway /api/pay/* ─▶ GET  /bills/{code}   (types the code, sees the bill)
                                  POST /bills/{code}   ──POST /transfers──▶ wallet-service
```

## Layout

```
cmd/payment/                        entrypoint: wires adapters into the core
internal/payment/
  core/
    domain/                         Intent (pending/processing/paid/expired), Item, errors
    port/                           inbound use cases + outbound repo, unit of work, wallet client
    service/                        use-case implementations
  adapter/
    inbound/http/                   REST handlers -> port.PaymentService
    outbound/postgres/              repository, unit of work, migrations (schema "payment")
    outbound/walletclient/          HTTP client for wallet-service
deployments/                        docker-compose (postgres + wallet + payment) + Dockerfile
```

## Run

```sh
# postgres + wallet (built from ../sds_wallet) + payment, all in docker
docker compose -f deployments/docker-compose.yml up -d --build
# wallet: localhost:8080, payment: localhost:8081, postgres: localhost:5434

# or: postgres + wallet in docker, payment locally
docker compose -f deployments/docker-compose.yml up -d db wallet
DATABASE_URL="postgres://sds:sds@localhost:5434/sds?sslmode=disable" \
WALLET_URL="http://localhost:8080" HTTP_ADDR=":8081" go run ./cmd/payment
```

PowerShell: `$env:DATABASE_URL="postgres://..."; $env:WALLET_URL="http://localhost:8080"; $env:HTTP_ADDR=":8081"; go run ./cmd/payment`

Raspberry Pi (arm64) image: `make image-arm64`. The Dockerfile cross-compiles, so no emulation is needed.

| Env var        | Default                                                 |
|----------------|---------------------------------------------------------|
| `HTTP_ADDR`    | `:8080`                                                 |
| `DATABASE_URL` | `postgres://sds:sds@localhost:5432/sds?sslmode=disable` |
| `DB_SCHEMA`    | `payment`                                               |
| `WALLET_URL`   | `http://localhost:8080`                                 |
| `BILL_TTL`     | `15m` (how long a customer has to pay a code)           |

Migrations run automatically at startup. They create the `payment` schema and
keep their own `payment.schema_migrations`, so the service can share one
database with wallet, auth and pos without mixing tables.

## Gateway requirements

- Route `/api/pay/*` to payment-service, stripping the `/api/pay` prefix.
- **Block `/api/pay/internal/*`.** Routes under `/internal/` are for pos-service only.
- Set `X-User-Id` from the JWT `sub` claim, and remove any `X-User-Id` sent by the client.

## API

Amounts are integers in satang (`10050` = ฿100.50). Request bodies are JSON;
unknown fields are rejected with **400**. Every route except `/healthz` needs
`X-User-Id`.

| Method | Path | Caller | Headers | Success |
|--------|------|--------|---------|---------|
| POST | [`/internal/bills`](#create-a-bill) | pos-service | `X-User-Id` (cashier) | 200 Cashier bill |
| GET  | [`/internal/bills/{id}`](#check-a-bill) | pos-service | `X-User-Id` (cashier) | 200 Cashier bill |
| GET  | [`/bills/{code}`](#look-up-a-bill-by-code) | customer | `X-User-Id` | 200 Bill |
| POST | [`/bills/{code}`](#pay-a-bill) | customer | `X-User-Id`, `Idempotency-Key` | 200 Receipt |
| GET  | `/healthz` | — | — | 200 `{"status":"ok"}` |

### Create a bill

`POST /internal/bills`. pos-service calls this for the cashier. `wallet_id`
is the shop's own wallet, from wallet-service `GET /wallets/me`.
payment-service asks wallet-service `GET /wallets/{wallet_id}` **as the cashier**,
so a wallet that isn't theirs gives **403**.

```json
{
  "bill_id": "B-1042",
  "restaurant_name": "Noodle House",
  "table": "5",
  "wallet_id": "0193a2c4-…",
  "items": [
    { "name": "Pad Thai", "qty": 2, "unit_price": 6000 },
    { "name": "Tom Yum Noodles", "qty": 1, "unit_price": 7500 }
  ]
}
```

`total` is computed from the items. `table` is optional.

Calling it again with the same `bill_id`:

| Current state of that bill | Result |
|---|---|
| Open, same contents | Same code (safe to retry) |
| Open, contents changed (e.g. an item was added) | New code; the old code becomes `expired` |
| Being paid right now | **422** `bill is being paid` |
| Paid | **422** `bill already paid` |
| Expired | New code |

### Check a bill

`GET /internal/bills/{id}`. pos-service polls this (e.g. every 2 s) to show
"paid" on the cashier screen. Only the cashier who created the bill can read it.

**Cashier bill**

```json
{
  "id": "0193a2d1-…",
  "bill_id": "B-1042",
  "code": "482913",
  "restaurant_name": "Noodle House",
  "table": "5",
  "items": [ … ],
  "total": 19500,
  "status": "paid",
  "expires_at": "2026-11-12T12:56:00Z",
  "created_at": "2026-11-12T12:41:00Z",
  "wallet_id": "0193a2c4-…",
  "transaction_id": "0193a2e7-…",
  "paid_at": "2026-11-12T12:43:08Z"
}
```

`status` is `pending`, `processing` (a customer is paying right now), `paid`
or `expired`. `transaction_id` and `paid_at` appear only once paid.

### Look up a bill by code

`GET /bills/{code}` (`/api/pay/bills/{code}` via the gateway). Same as the
Cashier bill without `id`, `wallet_id`, `transaction_id` and `paid_at`.
`status` is `pending`, `paid` or `expired`; a payment in progress shows as
`pending`. Paid and expired bills return **200** with that status. An unknown or
malformed code returns **404** `bill not found`.

### Pay a bill

`POST /bills/{code}` with an `Idempotency-Key` header.

```json
// request
{ "wallet_id": "9c1e…3f9a" }

// 200 response
{ "transaction_id": "0193a2e7-…", "paid_at": "2026-11-12T12:43:08Z", "amount": 19500, "balance_after": 98550 }
```

It calls wallet-service `POST /transfers` as the customer, from `wallet_id` to the
shop's wallet, with the note `Bill 482913 · Noodle House`. That note shows up in
the customer's wallet history.

Repeating the request with the same `Idempotency-Key` and body returns the same
receipt. With the same key and a different body, it returns **409**.

### Errors

```json
{ "error": "insufficient funds" }
```

| Status | When |
|--------|------|
| 400 | Bad JSON, unknown field, missing `bill_id` / `restaurant_name` / `wallet_id` / items, bad item, total ≤ 0 (`amount`), missing `Idempotency-Key`, paying into the same wallet |
| 401 | `X-User-Id` missing |
| 403 | The wallet or bill isn't the caller's |
| 404 | `bill not found`, `wallet not found` |
| 409 | `Idempotency-Key` reused with a different body |
| 422 | `insufficient funds`, `bill already paid`, `bill expired`, `bill is being paid`, `wallet is inactive`, `wallet currency must be THB` |
| 502 | `wallet service unavailable`: wallet-service down or timed out. Retrying with the same key is safe. |
| 500 | Unexpected error (`"internal server error"`) |

## How a bill is never charged twice

1. **Claim.** Under a row lock, a `pending` intent becomes `processing` and
   records the payer (user, wallet, `Idempotency-Key`). This commits **before**
   wallet-service is called, so a crash leaves a durable record and only one
   payment attempt is ever in flight.
2. **Transfer.** wallet-service gets the idempotency key `bill:<intent id>`,
   which is tied to the bill, not to the customer's request. Any replay of the
   transfer, even with a new customer key or from another replica, can move
   money at most once.
3. **Settle.**
   - Success: the intent becomes `paid`.
   - Definitive refusal (insufficient funds, not your wallet, frozen, …):
     wallet-service rolled back, so the intent goes back to `pending` and can
     be paid again.
   - Unknown (timeout, 5xx): the intent stays `processing`. The next pay request,
     or the cashier's poll after 10 s, replays the same transfer and finishes it.

A `processing` intent never expires, so a payment whose response was lost
isn't reported as `expired` after the money moved.

Codes are random 6-digit numbers. A code is unique only among live
(`pending`/`processing`) intents, enforced by a partial unique index, so codes
are reused over time. Lookup returns the newest intent with that code.

## Test

```sh
go test ./...            # unit tests; Postgres tests are skipped
make test-integration    # starts the docker postgres and runs everything
```

PowerShell:

```powershell
docker compose -f deployments/docker-compose.yml up -d db
$env:TEST_DATABASE_URL="postgres://sds:sds@localhost:5434/sds?sslmode=disable"; go test -count=1 ./...
```

| Package | What it covers |
|---|---|
| `core/domain` | totals, expiry, claim/release, codes, note length |
| `core/service` | create/re-create/replace bills, wallet ownership, pay, replay, refusals, expiry, lost wallet responses, stuck payments settled by polling, concurrent payers |
| `adapter/inbound/http` | routing, customer vs cashier views, body validation, error → status and keywords |
| `adapter/outbound/walletclient` | request shape, headers, wallet error → domain error mapping |
| `adapter/outbound/postgres` | migrations in own schema, round trip, live code/bill uniqueness, concurrent payers and creates across two service instances |
