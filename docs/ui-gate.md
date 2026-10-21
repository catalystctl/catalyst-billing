# Web UI gate — implementation specification

**Status: closed.** Every task below is implemented, tested against a real database, and
reachable through the route table. This document is kept as the record of *why* each rule
exists — the reasoning is the expensive part, and it is what a future change has to argue
against.

Two notes on reading it:

* Where the implementation departs from the text below, the departure is recorded in
  "Deviations found while implementing" at the end of the relevant gate, together with the
  bug that forced it. Two of those were real defects the specified design would have had.
* The contracts here are also generated: `api/openapi.yaml` is produced from the route table
  by `make openapi`, and a test fails if it drifts. When the prose below and the document
  disagree, the document is what the server does.

Every task was originally specified so it could be implemented without a design
conversation. Each entry states what already exists, the schema it needs, the endpoint
contract, the rules the implementation must enforce, the errors it must return, and the
tests that prove it.

Conventions used throughout, matching the existing code:

- Errors are `application/problem+json` via `writeProblem(w, status, code, detail)`:
  `{"type","title","status","detail","code"}`. `code` is stable and is what a client
  branches on.
- Money is always `{"minor": <int64>, "currency": "USD", "scale": 2, "display": "12.34 USD"}`
  (`amountJSON` in `internal/httpapi/portal.go`). Never a float, never a decimal string
  in a mutation body.
- A route declares its permission with
  `s.requirePermission(authn.PermX, handler)`; a mutation additionally declares
  `s.requireCSRF(...)`.
- Identity comes from the session (`userFrom(ctx)`), never from a path or body.
- Lists are cursor-paginated: `?limit=` (default 25, max 100) and `?cursor=`.
  Response: `{"data": [...], "next_cursor": "..." | null}`. The cursor is opaque,
  base64 of `(sort_key, id)`, and must be stable under concurrent inserts.
- Mutations accept `Idempotency-Key` (see E3).
- Long actions return `202` with `{"operation_id", "status_url"}` (see E5).

Notation: **Exists** = already implemented, **Add** = must be written.

> **Every `sql` block in this document is the body of a migration file** — the next
> free version number in `internal/store/migrations/` (`0010_*.sql` onwards) — applied
> by `billing migrate` inside one transaction and recorded with a checksum. They are
> never run by hand against a database: several blocks are `ALTER TABLE` on existing
> tables (`refunds`, `invoices`), and running one ad hoc against a live schema creates
> a column no migration knows about. That mistake was made while verifying this
> document, which is why the warning is here.
>
> All of the DDL below has been applied to a scratch schema and its constraints
exercised: `approvals_distinct_actors`, `credit_notes_issued_consistency`,
> `credit_notes_total_minor_check`, `audit_events_outcome_check`,
> `billing_profiles_country_code_check`, `password_resets_expiry`,
> `mfa_one_per_kind` and `notification_deliveries_dedupe_key_key` all fire on the
> input they are meant to refuse.

---

## Gate A — staff read surface

### A0. Checkout over HTTP (prerequisite for the customer "Pay" button)

**Exists:** `internal/checkout` is complete and tested — `checkout.New(pool, gateway,
account, successURL, cancelURL)`, `Service.Start(ctx, invoiceID) (Session, error)`,
`OperationKey(invoiceID)`, `AttemptForPayment`, `LookupFor`. It records the attempt
before calling the gateway, reuses an existing session instead of opening a second
one, and cancels the attempt when the gateway call fails. **It has no route**, so
nothing can pay.

**Add:** one route, no new logic.

```
POST /api/v1/portal/invoices/{id}/checkout
  permission: invoice:pay:self
  csrf: required
  body: {}                      # deliberately empty; the amount is never client-supplied
  200: {"attempt_id","invoice_id","redirect_url","amount":{...},"reused":bool}
  402: {"code":"not_payable"}   # draft, void, or nothing outstanding
  404: {"code":"not_found"}     # not this customer's, or malformed id
  409: {"code":"already_settled"}
  503: {"code":"gateway_unavailable"}
```

**Rules:**
- The amount comes from `Service.Start`, which reads `total_minor - amount_paid_minor`
  under a row lock. A body field for the amount must not exist.
- `checkout.Session.Reused == true` is a `200`, not an error: the customer clicked twice.
- `portal.ValidID` gates the path parameter before it reaches the database, so a
  malformed id is a `404` and never a `500` carrying SQL.
- If no gateway is configured (`s.gateway == nil`), return
  `503 gateway_unavailable` and log once at startup, not per request.

**Tests:** valid checkout returns a redirect and creates one `payment_attempts` row;
second call reuses it (gateway called once); settled invoice is `409`; another
customer's invoice is `404` with a body byte-identical to a nonexistent id; malformed
id is `404`; no gateway is `503`.

### A1. Staff customer list, detail, search

**Exists:** `customers` (id, email, name, created_at), `customer_accounts`
(user_id, customer_id, role), `services`, `invoices`, `subscriptions`. No staff read.

```
GET /api/v1/staff/customers
  permission: customer:read:all
  query: q (matches email or name, ILIKE, min 2 chars), status? (none today),
         limit, cursor, sort=created_at|email|name (default created_at desc)
  200: {"data":[{"id","email","name","created_at",
                 "open_invoices":int,"outstanding":[amount...],"services":int,
                 "has_login":bool}],"next_cursor":null}

GET /api/v1/staff/customers/{id}
  permission: customer:read:all
  200: {"customer":{...},"accounts":[{user_id,email,role,last_seen_at}],
        "subscriptions":[...],"services":[...],"invoices":[last 20],
        "balance":{"outstanding":[amount],"credit":[amount]},
        "holds":[{id,service_id,reason,expires_at}],
        "recent_auth_events":[last 10]}
  404: {"code":"not_found"}
```

**Rules:**
- `outstanding` is derived per currency (a customer can hold two), never summed
  across currencies. Reuse `portal.Manager.Balance` logic, not a copy.
- `has_login` is true when a `customer_accounts` row exists; it explains why a
  customer cannot see the portal, which is the most common support question.
- Search must be indexed: `CREATE INDEX customers_name_trgm ON customers USING gin (name gin_trgm_ops)`
  requires the `pg_trgm` extension, or use a `lower(email)` prefix index and accept
  that name search is a scan. Choose one and state it in the migration comment.
- A staff user without a `customer_accounts` row is a legitimate state, not an error.

**Tests:** search by email and by name; a customer with two currencies shows two
outstanding lines; a customer with no login shows `has_login:false`; detail includes
holds; permission denied for `support` on a write route but allowed on read.

### A2. Staff invoice list and detail

**Exists:** `invoices`, `invoice_lines`, `payments`, `payment_allocations`,
`journal_transactions`/`journal_entries`, `billing.Issuer`.

```
GET /api/v1/staff/invoices
  permission: invoice:read:all
  query: status (draft|open|paid|void|uncollectible|refunded|disputed),
         customer_id, overdue=true, currency, from, to (issued_at), limit, cursor
  200: {"data":[{"id","number","customer":{id,email},"status","currency",
                 "total":amount,"paid":amount,"due":amount,"issued_at","due_at",
                 "overdue":bool}],"next_cursor":null}

GET /api/v1/staff/invoices/{id}
  permission: invoice:read:all
  200: {"invoice":{...full row...,"snapshot":{...},"lines":[...]},
        "payments":[{provider,provider_payment_id,amount,received_at,status}],
        "allocations":[...],
        "refunds":[...],
        "credit_notes":[...],
        "journal":[{"kind","reference","entries":[{"account","debit","credit"}]}]}
```

**Rules:**
- `snapshot` is returned verbatim: it is the evidence of what was billed, and an
  operator debugging a total needs it.
- `overdue` is derived (`status='open' AND due_at < now()`), never stored.
- The journal section is read-only. There must be no endpoint that edits a posting;
  corrections are reversal postings.
- Never expose another tenant's data here — these routes are staff-only and carry
  `invoice:read:all`, which no customer role holds.

**Tests:** list filters by status and overdue; detail returns the snapshot and the
journal; a paid invoice's paid amount equals the sum of its allocations.

### A3. Staff service list and detail

```
GET /api/v1/staff/services
  permission: service:read:all
  query: status, customer_id, product_id, has_open_operation=true, limit, cursor
  200: {"data":[{"id","customer":{id,email},"product","status","external_id",
                 "generation","next_bill_at","open_operations":int,
                 "last_error":string|null}],"next_cursor":null}

GET /api/v1/staff/services/{id}
  permission: service:read:all
  200: {"service":{...},"subscription":{...},"operations":[last 20 with status,
        attempts, last_error, operation_key],"holds":[...],
        "eligibility":{"eligible":bool,"reason":string},"invoices":[...]}
```

**Rules:**
- `eligibility` is `dunning.Engine.Evaluate`, which records a `suspension_decisions`
  row. A read endpoint that writes is surprising, so **do not call it here**; instead
  expose a separate `GET .../suspension-eligibility` that the operator clicks, or
  accept the audit row and document it. Recommended: separate endpoint.
- `last_error` is the newest non-null `service_operations.last_error`, so the UI can
  explain a stuck service without a second request.

**Tests:** a service with a failed operation reports its error; a service with no
subscription shows `next_bill_at: null`; the eligibility endpoint records a decision.

### A4. Staff order and checkout list

**Exists:** `orders`, `order_items` (with `price_version_id`), `payment_attempts`.

```
GET /api/v1/staff/orders
  permission: invoice:read:all
  query: status, customer_id, limit, cursor
  200: {"data":[{"id","customer","status","currency","total":amount,
                 "created_at","invoice_id":string|null}],"next_cursor":null}

GET /api/v1/staff/orders/{id}
  permission: invoice:read:all
  200: {"order":{...},"items":[{"description","quantity","unit","line","setup",
        "price_version_id","provision_plan","provision_region"}],
        "invoices":[...],"attempts":[{"gateway","status","provider_ref","amount"}]}
```

**Rules:** `orders.status` is one of `pending_payment, paid, fulfilling, completed,
cancelled, needs_review`. Nothing currently transitions it after payment; the
implementation must decide and document: recommended is that the payment applier sets
`orders.status='paid'` in the same transaction, and the provisioning worker sets
`fulfilling` then `completed` when every item's operation succeeds. Without that, the
status column is decorative and the UI cannot show a truthful order state.

**Tests:** an order's detail lists its items with the price snapshot; after payment
the order is `paid`; after all operations succeed it is `completed`.

### A5. Staff payment and attempt history

```
GET /api/v1/staff/payments
  permission: payment:reconcile
  query: provider, status, from, to, customer_id, limit, cursor
  200: {"data":[{"id","provider","provider_account","provider_payment_id",
                 "amount","currency","status","received_at",
                 "invoice_ids":[...]}],"next_cursor":null}

GET /api/v1/staff/payments/{id}
  permission: payment:reconcile
  200: {"payment":{...},"allocations":[...],"refunds":[...],"journal":[...]}
```

**Rules:** a payment is immutable once recorded. There is no edit endpoint, and there
must not be one. A wrong amount is corrected by a refund or credit note, never by
changing the payment.

**Tests:** a payment lists its allocation and the invoice it settled; a refunded
payment shows `status='refunded'` and the refund rows.

### A6. Operation and job queue view

**Exists:** `service_operations`, `renewal_runs`, `renewal_events`, `gateway_events`.
`ops.Overview` already reports counts and exceptions.

```
GET /api/v1/staff/operations
  permission: service:read:all
  query: status (queued|running|succeeded|retry_wait|unknown|failed|needs_review),
         kind, service_id, limit, cursor
  200: {"data":[{"id","service":{id,customer},"kind","operation_key","generation",
                 "status","attempts","last_error","created_at","started_at",
                 "finished_at","lease_expires_at":string|null}],"next_cursor":null}

GET /api/v1/staff/webhooks
  permission: payment:reconcile
  query: processed=true|false, provider, limit, cursor
  200: {"data":[{"id","provider","event_id","event_type","received_at",
                 "processed_at","attempts","last_error"}]}
```

**Rules:** `lease_expires_at` is `started_at + worker.LeaseDuration`; the UI shows
"will be reaped in N minutes" so an operator knows whether to wait or intervene.

**Tests:** filtering by `unknown` returns exactly the unreconciled operations; a
processed webhook is excluded by `processed=false`.

### A7. Catalog read

```
GET /api/v1/staff/catalog/products
  permission: invoice:read:all
  200: {"data":[{"id","slug","name","price_versions":[{"id","version","currency",
        "amount","setup","interval","anchor_day","anchor_timezone",
        "provision_plan","provision_region","created_at"}]}]}

GET /api/v1/staff/catalog/products/{id}
  permission: invoice:read:all
  200: {"product":{...},"price_versions":[...],"active_subscriptions":int}
```

**Rules:** price versions are immutable. The editor's "save" creates version N+1; it
must never `UPDATE price_versions`. `active_subscriptions` explains why a version
cannot be deleted, which is the question the UI will ask.

**Tests:** saving a price creates a new version and leaves the old row byte-identical;
an invoice issued before the edit still totals the old price.

---

## Gate B — staff mutations

### B1. Service actions: suspend, resume, terminate

**Exists:** `dunning.Engine.RequestSuspension` (evaluates eligibility, enqueues a
generation-fenced operation), `RequestResume` (requires nothing owed),
`provisioning.Worker` (executes, fences, reconciles).

```
POST /api/v1/staff/services/{id}/suspend
  permission: service:suspend
  csrf: required, Idempotency-Key: required
  body: {"reason": string, "override_hold": bool}
  202: {"operation_id","status_url","reason_if_refused":null}
  409: {"code":"not_eligible","detail":"<reason from Evaluate>","outstanding":amount}
  409: {"code":"hold_in_place"}          # unless override_hold with a second approval

POST /api/v1/staff/services/{id}/resume
  permission: service:suspend
  body: {"reason": string}
  202: {"operation_id","status_url"}
  409: {"code":"still_owing","outstanding":[amount...]}

POST /api/v1/staff/services/{id}/terminate
  permission: service:terminate
  body: {"reason": string, "confirm":"<service external_id or id>"}
  202: {"operation_id","status_url"}
  400: {"code":"confirmation_mismatch"}
```

**Rules:**
- Suspend and resume **must** go through `dunning.Engine`, never by inserting a
  `service_operations` row directly: that is what guarantees the eligibility decision
  and the generation are recorded in `suspension_decisions`.
- Terminate is destructive and cannot be reconciled automatically (see
  `provisioning.Worker.reconcileOne`: it refuses without an external id). It requires
  the operator to echo the service's `external_id`; the response is `400` otherwise.
- `override_hold` requires `PermServiceTerminate` **and** a second distinct staff
  actor (`approvals`, see D4). An override is recorded as a `suspension_decisions`
  row with `decision='suspend'` and a reason naming both actors.
- Every one of these writes an `audit_events` row (F1) in the same transaction.

**Tests:** suspend on an exhausted invoice enqueues one operation; suspend twice
enqueues once; suspend on a held service is `409 hold_in_place`; terminate without a
matching confirmation is `400` and enqueues nothing; a stale generation is
`needs_review` and does not change service status.

### B2. Dunning hold create and clear

**Exists:** `dunning.Engine.AddHold(ctx, serviceID, reason, createdBy, expiresAt)`,
`ClearHold(ctx, holdID)`, `service_holds` table.

```
POST /api/v1/staff/services/{id}/holds
  permission: service:suspend
  body: {"reason": string (required, min 3 chars), "expires_at": RFC3339|null}
  201: {"id","service_id","reason","created_by","expires_at"}

GET /api/v1/staff/services/{id}/holds
  permission: service:read:all
  200: {"data":[{...}]}

DELETE /api/v1/staff/holds/{hold_id}
  permission: service:suspend
  204
  404: {"code":"not_found"}
```

**Rules:** `created_by` is the session's user id, never a body field. A hold with no
expiry is deliberate and must be shown as "permanent" in the UI, because it blocks
automation until a human clears it.

**Tests:** a hold blocks suspension; clearing it re-enables eligibility; an expired
hold does not block; `created_by` is the session user even when the body tries to set it.

### B3. Manual invoice issue and void

**Exists:** `billing.Issuer.CreateDraft` / `Issue`, `billing.DefaultSeries`, the CLI
`billing admin issue-invoice` (which is a thin wrapper over the same code).

```
POST /api/v1/staff/invoices
  permission: invoice:write:all
  body: {"customer_id","lines":[{"description","quantity_milli","unit_minor",
         "currency","tax_rate"}],"due_at":RFC3339|null,"terms":string,
         "issue_now":bool}
  201: {"invoice_id","number"|null,"totals":{"subtotal","tax","total"},"status"}
  400: {"code":"invalid_line"}  # empty description, quantity <= 0, mixed currency

POST /api/v1/staff/invoices/{id}/issue
  permission: invoice:write:all
  body: {}
  200: {"number","totals","issued_at"}
  409: {"code":"not_draft"}

POST /api/v1/staff/invoices/{id}/void
  permission: invoice:write:all
  body: {"reason": string}
  200: {"status":"void"}
  409: {"code":"not_voidable"}   # a paid invoice cannot become void (DB trigger)
```

**Rules:**
- Amounts are `unit_minor` integers; the server computes totals with
  `billing.FixedTaxPricer`, never accepts a total from the client.
- `issue_now` performs `CreateDraft` then `Issue` in one transaction, so a failure
  leaves no draft behind.
- Voiding does not delete the invoice and does not reverse its posting. If money was
  received, the correction is a credit note (D2); the trigger in `0008_invariants.sql`
  already refuses `paid -> void`.
- A voided invoice releases its `(subscription_id, period_key)` slot, so a corrected
  renewal can be issued for the same period. That is already enforced by the partial
  unique index.

**Tests:** issuing allocates the next number; issuing twice is `409`; a paid invoice
cannot be voided; voiding a renewal lets the same period be reissued; totals match the
pricer's own computation to the minor unit.

### B4. Billing profile write

**Exists:** nothing. `billing.Snapshot.Customer` carries `BillingAddress`, `TaxID`,
`CountryCode`, but no table stores them per customer.

**Add schema:**

```sql
CREATE TABLE billing_profiles (
    customer_id     uuid PRIMARY KEY REFERENCES customers (id),
    legal_name      text NOT NULL DEFAULT '',
    address_line1   text NOT NULL DEFAULT '',
    address_line2   text NOT NULL DEFAULT '',
    city            text NOT NULL DEFAULT '',
    postal_code     text NOT NULL DEFAULT '',
    region          text NOT NULL DEFAULT '',
    country_code    text NOT NULL CHECK (country_code = '' OR country_code ~ '^[A-Z]{2}$'),
    tax_id          text NOT NULL DEFAULT '',
    updated_at      timestamptz NOT NULL DEFAULT now()
);
```

```
GET  /api/v1/portal/billing-profile    permission: customer:read:self
PUT  /api/v1/portal/billing-profile    permission: customer:write:self, csrf
  body: {legal_name,address_line1,address_line2,city,postal_code,region,
         country_code,tax_id}
  200: {profile}
  400: {code:"invalid_profile"}   # country_code not ISO-3166 alpha-2, etc.

GET  /api/v1/staff/customers/{id}/billing-profile   permission: customer:read:all
PUT  /api/v1/staff/customers/{id}/billing-profile   permission: customer:write:all
```

**Rules:** the profile is *input* to the next invoice, never a rewrite of a past one.
`billing.Issuer` reads it into `Snapshot.Customer` at issuance; an issued invoice's
snapshot is immutable by trigger. A tax id is stored as given, never validated
against a jurisdiction — that would imply a compliance claim the product does not make.

**Tests:** updating the profile does not change an existing invoice's snapshot; the
next invoice carries the new details; an invalid country code is rejected.

---

## Gate C — account lifecycle

### C1. Password reset

**Add schema:**

```sql
CREATE TABLE password_resets (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash  bytea NOT NULL,
    expires_at  timestamptz NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT password_resets_expiry CHECK (expires_at > created_at)
);
CREATE UNIQUE INDEX password_resets_token_unique ON password_resets (token_hash);
CREATE INDEX password_resets_user_idx ON password_resets (user_id);
```

```
POST /api/v1/auth/password-reset/request
  body: {"email"}
  202: {"status":"accepted"}          # ALWAYS, whether or not the account exists
  # 5 requests per email per hour, 20 per address per hour

POST /api/v1/auth/password-reset/complete
  body: {"token","new_password"}
  200: {"status":"password_changed"}  # all sessions revoked by credential_version bump
  400: {"code":"invalid_token"}       # unknown, used, or expired: one message for all three
  422: {"code":"weak_password"}       # < authn.MinPasswordLength
```

**Rules:**
- The request endpoint **must** take the same time and return the same body for a
  known and an unknown address. It also performs the dummy hash for an unknown
  address, exactly as login does.
- Token: 32 random bytes, base64url, stored as `authn.HashToken`, single use, 30
  minute expiry. Using it sets `used_at` and bumps `users.credential_version` (which
  revokes every session), in one transaction.
- The response never contains the token. Delivery is C3.

**Tests:** request for an unknown address returns `202` and sends nothing; a valid
token changes the password once and the second use is `400`; a reset revokes existing
sessions; an expired token is `400`; the response body for known and unknown addresses
is byte-identical.

### C2. Email verification

**Exists:** `users.email_verified_at`, never set.

```
POST /api/v1/auth/verify/request        (authenticated) 202
GET  /api/v1/auth/verify?token=...      200 {"status":"verified"} | 400 invalid_token
```

**Rules:** 24 hour expiry, single use, 3 requests per hour. Verification is *not*
required to sign in (that would lock out an operator who created an account by CLI);
it is required before C4's MFA enrolment and before the account may pay.

**Tests:** verifying twice is `400` the second time; an unverified account can still
sign in; `me` reports `email_verified` accurately.

### C3. Notification delivery (SMTP)

**Add schema:**

```sql
CREATE TABLE notification_deliveries (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL,            -- password_reset, email_verification, invoice_issued, dunning_reminder
    to_address   text NOT NULL,
    subject      text NOT NULL,
    -- The rendered body is stored so "what did we actually send" is answerable.
    body         text NOT NULL,
    status       text NOT NULL CHECK (status IN ('queued','sending','sent','failed','suppressed')),
    attempts     int NOT NULL DEFAULT 0,
    last_error   text,
    dedupe_key   text UNIQUE,              -- e.g. invoice_issued:<invoice_id>
    created_at   timestamptz NOT NULL DEFAULT now(),
    sent_at      timestamptz
);
CREATE INDEX notification_deliveries_pending_idx ON notification_deliveries (created_at)
    WHERE status IN ('queued','failed');
```

```
# Interface in internal/notify:
type Sender interface {
    Send(ctx context.Context, to, subject, body string) error
}
```

**Rules:**
- Delivery is *durable first*: a row is written in the same transaction as the
  domain change (invoice issued, reset requested), and a worker sends it. A crash
  between the two cannot lose the message, and a retry cannot send twice because
  `dedupe_key` is unique.
- `suppressed` is used when the address is unverified and the message kind requires
  verification.
- Body templates are plain text with no user HTML: an invoice line description is
  attacker-controlled and must not be rendered as markup.
- The SMTP sender must not block a request: `queued` is written, the request returns.

**Tests:** issuing an invoice queues exactly one `invoice_issued` row; replaying the
same invoice does not queue a second (`dedupe_key`); a failing sender leaves
`status='failed'` with the error and retries with backoff; a `queued` row survives a
worker restart.

### C4. MFA enrolment (TOTP)

**Add schema:**

```sql
CREATE TABLE mfa_credentials (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind         text NOT NULL CHECK (kind IN ('totp')),
    secret_enc   bytea NOT NULL,          -- encrypted with the core key, never plaintext
    key_id       text NOT NULL,           -- which key encrypted it, for rotation
    confirmed_at timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT mfa_one_per_kind UNIQUE (user_id, kind)
);
CREATE TABLE mfa_recovery_codes (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id   uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash bytea NOT NULL,
    used_at   timestamptz
);
```

```
POST /api/v1/auth/mfa/enrol            (auth) 200 {"secret","otpauth_url"}  # unconfirmed
POST /api/v1/auth/mfa/confirm          body {"code"} 200 {"recovery_codes":[10 codes, shown once]}
POST /api/v1/auth/mfa/disable          body {"password","code"} 204
# login second step:
POST /api/v1/auth/login                → when MFA is confirmed:
  200: {"mfa_required":true,"challenge_token":...}      # NO session cookie yet
POST /api/v1/auth/mfa/verify           body {"challenge_token","code"} 200 {session}
```

**Rules:**
- `challenge_token` is a short-lived (5 min) single-use token that proves the password
  step passed. It is **not** a session and grants no access.
- The secret is encrypted with the same key mechanism as gateway secrets (key id +
  authenticated encryption). It is never logged and never returned after enrolment.
- Recovery codes are hashed like API keys and each is single use.
- Staff MFA is enforced by policy: `staff_roles` + confirmed MFA is required before
  any `*:all` permission is honoured, gated by a config flag so the first bootstrap
  account can enrol.

**Tests:** login with a confirmed credential returns a challenge and no session; a
wrong code is `401` and does not consume the challenge; a valid code issues a session;
a recovery code works once; the secret is not readable in plaintext from the database.

### C5. API keys

**Add schema:**

```sql
CREATE TABLE api_keys (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name         text NOT NULL,
    prefix       text NOT NULL,           -- first 8 chars, shown in the UI
    key_hash     bytea NOT NULL,          -- SHA-256 of the full key
    scopes       text[] NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz
);
CREATE UNIQUE INDEX api_keys_hash_unique ON api_keys (key_hash);
```

```
POST   /api/v1/staff/api-keys    permission: apikey:manage  body {"name","scopes","expires_at"}
  201: {"id","name","prefix","key":"bk_<43 chars>","scopes","expires_at"}   # shown ONCE
GET    /api/v1/staff/api-keys    permission: apikey:manage  200 {data:[{...no key...}]}
DELETE /api/v1/staff/api-keys/{id}  permission: apikey:manage  204
```

**Rules:** the key is generated server-side, returned once, stored only as a hash, and
scoped. Authentication via `Authorization: Bearer <key>` resolves to the owning user
and intersects the key's `scopes` with that user's permissions — a key can never grant
more than its owner holds, and revoking the user's role immediately narrows the key.

**Tests:** the key is returned once and never again; a revoked key is `401`; a key
whose owner lost a permission loses it too; an expired key is `401`.

---

## Gate D — money actions

### D1. Refund

**Exists:** `refunds` (id, payment_id, provider_refund_id, operation_key,
amount_minor, currency, status), bounds enforced (`amount_minor > 0`), and
`ErrPaymentReused` guards. No flow.

**Add schema (columns on the existing table — this is a migration file, `0011_refunds.sql`):**

```sql
ALTER TABLE refunds
    ADD COLUMN reason        text NOT NULL DEFAULT '',
    ADD COLUMN requested_by  uuid REFERENCES users (id),
    ADD COLUMN approved_by   uuid REFERENCES users (id),
    ADD COLUMN finished_at   timestamptz;
-- One refund attempt per logical request.
CREATE INDEX refunds_payment_idx ON refunds (payment_id);
```

```
POST /api/v1/staff/payments/{id}/refunds
  permission: payment:refund
  csrf, Idempotency-Key
  body: {"amount":{"minor":1200,"currency":"USD"},"reason":string}
  201: {"id","status":"created","amount":{...},"refundable_remaining":{...}}
  409: {"code":"exceeds_refundable","refundable":amount}
  409: {"code":"already_refunded","refundable":amount}
  409: {"code":"approval_required"}       # over the threshold (D4)
  502: {"code":"provider_error"}          # nothing recorded, safe to retry
  202: {"id","status":"unknown"}          # provider did not answer: see rules

GET /api/v1/staff/refunds/{id}    permission: payment:refund
```

**Rules (the part that must not be improvised):**
- **Refundable amount** = `payments.amount_minor - sum(succeeded|pending refunds)`.
  Compute it under `SELECT ... FROM payments WHERE id=$1 FOR UPDATE` so two operators
  cannot each refund the full amount.
- **Posting**: a refund is a new journal transaction
  (`kind='refund'`, `reference_type='refund'`, `reference_id=<refund id>`) that debits
  `refunds` (expense) and credits `cash_clearing`, in the payment's currency. It does
  **not** touch the invoice's `amount_paid_minor`: the customer did pay, and the
  invoice's history is immutable.
- **Unknown outcome** is handled exactly like provisioning: the refund row is created
  with `status='unknown'` *before* the provider call, the operation key is stable, and
  a reconciler queries the provider rather than retrying the refund. Retrying a refund
  blindly refunds twice.
- A refund never creates customer credit. Credit is D3, and it is a separate decision.

**Tests:** refund of the full amount leaves `refundable=0`; a second refund is `409`;
a partial refund twice sums correctly; a timeout leaves `unknown` and no posting; the
reconciler resolves it once; the ledger still sums to zero in every currency;
concurrent full-amount refunds produce exactly one.

### D2. Credit note

**Add schema:**

```sql
CREATE TABLE credit_notes (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    number         text UNIQUE,
    customer_id    uuid NOT NULL REFERENCES customers (id),
    invoice_id     uuid REFERENCES invoices (id),
    currency       text NOT NULL CHECK (currency ~ '^[A-Z0-9]{2,12}$'),
    total_minor    bigint NOT NULL CHECK (total_minor > 0),
    reason         text NOT NULL,
    status         text NOT NULL CHECK (status IN ('draft','issued','applied','void')),
    issued_at      timestamptz,
    -- Frozen evidence, same idea as an invoice snapshot.
    snapshot       jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT credit_notes_issued_consistency CHECK (
        (status = 'draft' AND number IS NULL AND issued_at IS NULL) OR status <> 'draft'
    )
);
```

```
POST /api/v1/staff/invoices/{id}/credit-notes
  permission: invoice:write:all
  body: {"amount":{"minor","currency"},"reason":string,"apply_to_balance":bool}
  201: {"id","number","status":"issued","amount":{...}}
  409: {"code":"exceeds_invoice","remaining":amount}
```

**Rules:**
- The credit note's number comes from its own series (`CN-`), allocated the same way
  as invoices, in the issuing transaction.
- Posting: debit `revenue` (and `tax_payable` for the tax share) and credit
  `customer_credit`, or credit `ar` when `apply_to_balance` is false and the credit is
  a refundable balance.
- Total credits against an invoice may not exceed its total; enforced in the service
  under a lock on the invoice.
- An issued credit note is immutable, same trigger pattern as invoices.

**Tests:** a credit note reduces what is owed; it cannot exceed the invoice; the
journal balances; the number series is gap-free under concurrency; an issued credit
note cannot be edited.

### D3. Customer credit application

**Exists:** `customer_credit` ledger account, `portal.Balance.Credit` reads it. Nothing
writes it except the proposed credit note.

```
POST /api/v1/staff/customers/{id}/credit/apply
  permission: invoice:write:all
  body: {"invoice_id","amount":{"minor","currency"}}
  200: {"applied":amount,"invoice":{"status","amount_paid"},"credit_remaining":amount}
  409: {"code":"insufficient_credit","available":amount}
```

**Rules:** applying credit is a journal transaction
(`kind='adjustment'`, `reference_type='invoice'`, `reference_id=<invoice>`) that debits
`customer_credit` and credits `ar`. It updates `invoices.amount_paid_minor` and may
transition the invoice to `paid`. It must be idempotent on `(invoice, credit
transaction)`; a repeated application must not double-spend the credit.

**Tests:** credit applies once; a second application beyond the balance is `409`; the
invoice becomes `paid` exactly when the balance reaches zero; the ledger balances.

### D4. Dual approval for high-value actions

**Add schema:**

```sql
CREATE TABLE approvals (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action        text NOT NULL,          -- refund, terminate_batch, hold_override
    reference_id  uuid NOT NULL,
    requested_by  uuid NOT NULL REFERENCES users (id),
    approved_by   uuid REFERENCES users (id),
    status        text NOT NULL CHECK (status IN ('pending','approved','rejected','expired')),
    reason        text NOT NULL DEFAULT '',
    created_at    timestamptz NOT NULL DEFAULT now(),
    decided_at    timestamptz,
    -- The second actor must be a different person.
    CONSTRAINT approvals_distinct_actors CHECK (approved_by IS NULL OR approved_by <> requested_by)
);
```

```
GET  /api/v1/staff/approvals                  permission: audit:read
POST /api/v1/staff/approvals/{id}/approve     permission: <the action's permission>
POST /api/v1/staff/approvals/{id}/reject      permission: <the action's permission>
```

**Rules:** the threshold is configuration, not code:
`BILLING_REFUND_APPROVAL_THRESHOLD_MINOR` (default `50000`, i.e. 500.00) and
`BILLING_TERMINATION_BATCH_APPROVAL` (default `10`). Above the threshold the mutation
returns `409 approval_required` with the created approval's id; the action is only
performed when `approved_by` is set by a different user holding the permission. The
`CHECK` above is the last line of defence: one person cannot approve their own action.

**Tests:** a refund above the threshold is refused and creates a pending approval; the
same user cannot approve it; a second user can, and the refund then proceeds; below
the threshold no approval is created.

---

## Gate E — contracts

### E1. OpenAPI document

**Add:** `api/openapi.yaml`, generated from handler annotations or maintained by hand
and **verified against the router**: a test enumerates `mux` patterns and asserts each
appears in the spec, and each spec path resolves to a route. Without that test the spec
rots silently.

**Done when:** `make openapi-check` fails if a route is undocumented or a documented
route is missing; a typed client is generated in CI from the spec and compiles.

### E2. Pagination

**Exists:** `internal/portal` lists all invoices with no limit.

**Add:** a shared helper in `internal/httpapi/paginate.go`:

```go
type Page[T any] struct {
    Data       []T    `json:"data"`
    NextCursor *string `json:"next_cursor"`
}
func ParsePage(r *http.Request) (limit int, cursor string, err error)   // limit default 25, max 100
func EncodeCursor(sortKey string, id string) string
func DecodeCursor(c string) (sortKey, id string, err error)
```

**Rules:** keyset pagination, never `OFFSET`. The cursor encodes the last row's
`(sort_key, id)`; the query becomes
`WHERE (sort_key, id) < ($1, $2) ORDER BY sort_key DESC, id DESC LIMIT $3`. A malformed
cursor is `400 invalid_cursor`, not a silent reset to page one.

**Tests:** page two does not repeat or skip rows when a row is inserted between pages;
`limit=0` and `limit=1000` are clamped; a malformed cursor is `400`.

### E3. Scoped idempotency keys

**Add schema:**

```sql
CREATE TABLE idempotency_keys (
    key           text NOT NULL,
    principal_id  uuid NOT NULL,
    route         text NOT NULL,
    request_hash  bytea NOT NULL,
    response_code int NOT NULL,
    response_body jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    PRIMARY KEY (principal_id, key)
);
```

**Rules:** the key is bound to `(principal, route, request digest)`. A replay with the
same digest returns the stored response verbatim. A replay with a *different* digest is
`422 idempotency_key_reuse` — never the stored response, because that would answer a
different question with a stale answer. Retention 24 hours; the sweeper deletes expired
rows (add `idempotency_keys` to `retention.allowedTables`).

**Tests:** same key twice performs one effect and returns the same body; same key with a
different body is `422`; a different principal with the same key is independent.

### E4. Uniform error contract

**Exists:** `writeProblem` on some paths; the portal and staff handlers must all use it.

**Add:** a test that enumerates every registered route, issues a request that fails
validation, and asserts the body parses as `{type,title,status,detail,code}` with a
non-empty `code`. That test is what keeps the contract uniform as routes are added.

### E5. Operation resource for long actions

**Add:** the shape `202 {"operation_id","status_url"}` where `status_url` is
`/api/v1/staff/operations/{id}`, plus `GET` on that path returning
`{id,kind,status,attempts,last_error,lease_expires_at}`. No HTTP connection is held open
while a remote call runs.

**Tests:** an action that enqueues returns `202` immediately, before the operation
completes; polling the status URL reflects the transition.

### E6. SSE for job and service updates

**Add:** `GET /api/v1/events` (`text/event-stream`), authenticated, filtered to the
caller: a customer receives events for their own services and invoices only; staff
receive events they hold a permission for. Events carry a monotonic `id:` so a client
can resume with `Last-Event-ID`. Only **committed** state is published — the publisher
reads after commit, never inside the transaction. Heartbeat comment every 20s so a
proxy does not idle the connection out.

**Tests:** an event is delivered after the transaction commits and not before; a
customer never receives another customer's event; reconnecting with `Last-Event-ID`
replays nothing that was already delivered.

---

## Gate F — trust surface

### F1. Application audit log

**Add schema:**

```sql
CREATE TABLE audit_events (
    id             bigserial PRIMARY KEY,
    actor_id       uuid REFERENCES users (id),
    actor_kind     text NOT NULL CHECK (actor_kind IN ('user','api_key','system')),
    action         text NOT NULL,           -- e.g. invoice.void, refund.create
    target_type    text NOT NULL,
    target_id      uuid,
    outcome        text NOT NULL CHECK (outcome IN ('success','failure','denied')),
    -- Redacted diff: field names and old/new values, with secrets removed.
    changes        jsonb NOT NULL DEFAULT '{}'::jsonb,
    correlation_id text NOT NULL DEFAULT '',
    ip             inet,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_target_idx ON audit_events (target_type, target_id, created_at DESC);
CREATE INDEX audit_events_actor_idx  ON audit_events (actor_id, created_at DESC);
```

**Rules:** written by the **domain services** inside the same transaction as the change,
not by handlers — a handler can be forgotten, a service cannot. Secrets are never
recorded: the redactor runs over `changes` before insert. The table is append-only
(same `reject_mutation` trigger as the ledger). `GET /api/v1/staff/audit`
(`permission: audit:read`) is paginated and filterable by actor, action, target and
date.

**Tests:** voiding an invoice writes one row with the actor and the reason; a denied
authorization writes `outcome='denied'`; an update to `audit_events` fails; a secret
field is redacted.

### F2. Rate limiting per principal

**Add:** a fixed-window counter per `(principal, route-class)` in PostgreSQL or in
process, with the limit in configuration. `429` with `Retry-After`. Login keeps its own
existing throttle. The limit must be documented per route class (reads, mutations,
exports).

**Tests:** exceeding the limit returns `429` with `Retry-After`; the window resets; two
principals do not share a bucket.

### F3. Permission coverage test

**Add:** a test that walks the router, extracts each route's declared permission from a
registry the router builds (change `requirePermission` to record into a map), and fails
if any route under `/api/v1/staff` or `/api/v1/portal` has none. That is the mechanism
that stops a future route shipping unguarded.

---

## Gate G — serving the UI

### G1. Static asset serving and embed

**Add:** `//go:embed dist` in a `web` package, served by `billing serve` at `/`, with
hashed asset names and immutable caching for hashed files, `no-store` for `index.html`.
SPA fallback to `index.html` for unknown non-`/api` paths only — an unknown `/api` path
must stay a JSON `404`.

### G2. CSP and security headers

**Add:** applied by the static handler:

```
Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self';
  img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none';
  form-action 'self'; object-src 'none'
X-Content-Type-Options: nosniff
Referrer-Policy: same-origin
Strict-Transport-Security: max-age=31536000; includeSubDomains   (only when Secure)
```

**Tests:** a browser test asserts the policy is present and that an inline script is
refused; `/api/unknown` still returns JSON.


---

# Deviations found while implementing

Three departures from the specification above, each forced by a defect the tests found. They
are recorded here rather than silently applied, because a future reader comparing the two
would otherwise think the document was wrong all along.

## D1 — the refundable calculation must count the intent row

The specification said the refundable balance is the payment amount minus refunds in
`succeeded` and the in-flight states. That is not sufficient, because the design also
requires the intent row to be committed **before** the provider is called. Between those two
moments the row is `created`, which the specified set excluded.

The consequence, found by `TestConcurrentRefundsProduceOne`: two simultaneous full-amount
refunds each computed the whole balance as available, and both proceeded. The customer was
refunded twice.

The fix is to include `created` in the sum. `failed` stays excluded, because a definitive
refusal means nothing moved and the balance is genuinely available again.

## D2 — the operation key must not be derived from the amount

The specification derived the refund's provider operation key from
`(payment, amount, requester)` so a retry would be a replay. That makes two *deliberate*
partial refunds of the same amount indistinguishable: the second is treated as a replay of
the first and silently does nothing, so the operator sees a success and the customer receives
half of what was promised.

The key now comes from the request's `Idempotency-Key`. A caller that wants retry safety
supplies the same key twice; one that wants a second refund supplies a new one. When absent, a
random key is generated and recorded, so the reconciler can still ask the provider the same
question later.

## D3 — `suspension_decisions` needed actor columns

The specification had the dunning engine record a decision, but the table had no columns for
*who* decided. That was fine while every decision was automatic. A hold override is the first
decision a human can make, and a human decision with no named human is not auditable — the
next incident review asks "who approved suspending a customer who was in a dispute" and the
answer has to be a person.

Migration `0017_hold_override.sql` adds `decided_by`, `approved_by` and `override_hold`, with a
`CHECK` that the two actors differ and that an override names both. The engine takes an
`audit.Auditor` so the row commits inside the same transaction as the operation.

## Also worth knowing

* **Audit attribution.** The audit row for a money action is written by the domain service
  inside its own transaction, through an `audit.Auditor` the HTTP layer supplies. Without that
  the row had no correlation id and no source address, which are the fields an incident review
  actually uses. A password reset is attributed to the account whose token was redeemed, not to
  `system`.
* **Customer search.** `pg_trgm` is used when it can be installed, with a `lower(name)` prefix
  index as the fallback, so a deployment that cannot install extensions still migrates.
* **The event broker is process-local.** Documented as a limit rather than papered over: a
  correct multi-instance feed needs a durable log and a per-client cursor.
