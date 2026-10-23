# Reachability — what is served, what is reachable, and what is a dead end

**The finding this document exists for.** The first run found that the purchase funnel was
API-only: nothing in the frontend ever called `POST /portal/cart/items`, the product page's buy
button forgot which product had been clicked, and a signed-in customer was redirected away from the
storefront. Every endpoint worked and was tested. A customer could not buy anything, so every order
in the business was an operator retyping a phone call into `/orders/new`.

That is the shape of the defect a route diff finds and a backlog never does: a working endpoint with
no button is indistinguishable from no endpoint, and no amount of reading the epics surfaces it. It
is now fixed, driven end to end by the harness, and `scripts/reachability.sh --check` fails if it
comes back.

**Status: open, and repeatable.** The sweep is `scripts/reachability.sh`; `--check` fails on a
finding that is not fixed or recorded below. The browser half is `scripts/e2e-routes.mjs`,
run by `scripts/browser-e2e.sh`, which opens every route in `web/src/routes.ts` and fails if
one of them is a dead end.

This document is the record, not the audit. The audit is the script: it rebuilds the served
route table from the router on every run and re-diffs it against the frontend, so the list
below cannot go stale without the command failing.

```
scripts/reachability.sh            # the report
scripts/reachability.sh --check    # exit 1 on a finding that is not fixed or recorded
scripts/reachability.sh --json     # machine-readable
bash scripts/browser-e2e.sh        # the named screens, then every route
make money-e2e                     # click the money documents: downloads, note, proforma, server CRUD, unlink
E2E_STRICT_PLACEHOLDERS=1 bash scripts/browser-e2e.sh   # a release check: placeholders fail too
```

**Baseline (last run):** 460 named-screen assertions green, 40 screens present, 0 broken, 0
reachability gaps, 78 routes rendered (76 in the table), and the funnel driven end to end.

## The method

The panel has two route tables, written by different people in different languages, and
nothing forces either to keep up with the other:

* **what is served** — `internal/httpapi` registers routes as data (`Route` in
  `internal/httpapi/routes.go`). `httpapi.DescribeRoutes()` projects that table without a
  database, and it is the same projection `make openapi` uses, so "served" and "documented"
  are already the same set.
* **what is opened** — `web/src/routes.ts` is the SPA's route table, `web/src/ui/layout.ts` is
  the navigation generated from it, and the views under `web/src/views/` call the API with
  path literals.

The sweep reads both sides and reports seven kinds of gap:

| Check | The defect it finds |
|---|---|
| API surface with no screen | A feature that is served, tested and documented, and that no operator can reach. |
| Write endpoint with no UI affordance | A mutation the panel never calls: a screen reads the group but offers no way to act. |
| Frontend call with no served route | A button that 404s: a view calls an endpoint the server does not serve. |
| Screen with no way in | A route no navigation entry, link or redirect points at, so only a URL reaches it. |
| Navigation entry with no route | A menu entry for an undeclared route, which `hasRoute` silently drops. |
| Permission gate this build never declares | A screen gated on a permission no role can hold, so it renders its refusal for everyone. |
| Navigation permission this build never declares | A menu entry filtered by a misspelled permission, hidden from every role. |

Matching is at **group granularity**. A route's *surface* is its static prefix
(`/api/v1/staff/coupons/{id}/redemptions` → `/api/v1/staff/coupons`), and a view that reads
the group counts as covering it. That is deliberate: a detail path is built from a row's id at
click time, so judging by the full pattern would report every detail endpoint as invisible. The
cost is that a sub-route can hide behind a sibling; the write check is the second net, because
it resolves the method at the call site.

## Findings

47 findings across 314 served routes, 75 screens and 55 navigation entries. Every one is
either fixed in this pass or recorded with the reason it is deliberate or belongs to another
owner. The script is the authoritative list; this is the reading of it.

### The purchase funnel is API-only — the largest gap in this report

`POST /api/v1/portal/cart/items` has **no caller anywhere in the web app**. Three defects
combine into one dead end:

1. `/products/:slug` renders a public product page whose "Order this plan" button links to
   `/login?next=/portal` — not to the product, and not to the cart. After signing in the
   visitor lands on the portal dashboard with nothing remembered.
2. A signed-in customer cannot browse the storefront at all: `app.ts` redirects every public
   path to the session's home, and `/products` is public.
3. `/cart` tells the customer to "pick a plan from the storefront and it appears here", which
   no screen can do.

The cart, checkout, order placement and payment all work — they are tested with a cart line
the browser harness inserts with SQL. A real customer has no path from a product to that cart.
**Recorded, not fixed here:** the fix spans `app.ts` (the public-path redirect),
`routes.ts`/`layout.ts` (a store destination for customers) and the storefront product page.
That is a product decision and three files owned elsewhere; it is reported to the orchestrator.

### Fixed in this pass

| Gap | The fix |
|---|---|
| `PUT /api/v1/staff/api-keys/{id}/ip-allowlist` had no UI: a key's CIDR allowlist was enforced by the server and could only be set with curl. | `web/src/views/api-keys.ts` now has a **Networks** action per live key. It reads the current rule, edits it as comma-separated CIDRs, and says plainly that a request from any other address is refused as if the key did not exist. Empty means no restriction. The sweep no longer reports the route. |
| `GET /api/v1/staff/credit-notes/{id}/pdf` and `GET /api/v1/portal/credit-notes/{id}/pdf` had no link: an operator could issue a credit note and not download it, and a customer was sent one they could not open. | The staff invoice screen's credit-note table has a **PDF** action per note. The portal needed a read first — nothing told a customer which notes existed — so `GET /api/v1/portal/invoices/{id}/document` now lists them (scoped by `customer_accounts.user_id` in the SQL) and `web/src/views/portal-invoice.ts` renders a credit-note section with the same download. The portal also gains the invoice download, from the same read. |
| `PUT /api/v1/staff/invoices/{id}/client-note` and `POST /api/v1/staff/invoices/{id}/proforma` had no control on the invoice screen. | `web/src/views/invoice.ts` has **Client note** (a dialog that says whether the installation prints notes, and that a note is stored when it does not) and **Issue proforma** on a draft. The staff invoice payload now carries `client_note`, `proforma`, `void_reason` and `notes_client_visible`, so the screen shows what it wrote and can tell a proforma from a numbered invoice. The customer's note is printed on their portal invoice as well as on the PDF. |
| `PUT` and `DELETE /api/v1/staff/catalog/servers/{id}` had no screen: a catalogue server could be created by API and never renamed or removed. | `web/src/views/catalog-admin.ts` has a **Servers** card: create, edit (rename, region, node, enable) and delete, with the delete refused while products reference the target — the dialog says so before the server refuses. |
| `DELETE /api/v1/portal/social-links/{id}` had no button: a provider could be linked and never unlinked. | The **Your own linked accounts** card on `/providers` has an **Unlink** action per identity, behind a confirmation that says the password still works. |

### API surfaces with no screen (27)

**Deliberate — no human is the audience:**

| Surface | Why |
|---|---|
| `GET /healthz`, `GET /readyz`, `GET /metrics` | probes and a Prometheus scrape; the dashboard shows health, not the probe |
| `POST /webhooks/{provider}`, `POST /api/v1/webhooks/mail/piping` | inbound callbacks, authenticated by signature; their outcome is on `/webhooks` and `/logs` |
| `GET /` | the SPA shell itself, not an API |
| `GET /api/v1/media/{id}` | an `<img src>`; the catalog screens render the images |
| `GET /api/v1/me`, `GET /api/v1/me/invoices` | credential introspection for API keys; the panel has a session, not a key |
| `GET/POST /oauth/authorize`, `POST /oauth/token` | OAuth2 for third-party clients; no human is in the panel |
| `GET /oauth/social/{slug}/start`, `GET /oauth/social/{slug}/callback` | browser redirects; the sign-in screen follows the `start_url` the server returns |
| `GET /api/v1/extensions/hooks` | the hook catalogue answers an extension asking what it may observe |
| `GET /api/v1/catalog/category/{slug}` | the storefront filters in place with `?category=`; a category URL is not part of its design |
| `GET /api/v1/staff/refunds/{id}` | the refund is read from its payment and rendered in the refund dialog |

**Reached through a constructed path** (the diff cannot see it, the screen exists):

| Surface | How the screen reaches it |
|---|---|
| `GET /api/v1/portal/tickets/meta` | `ticketsMetaPath()` in `views/tickets.ts` |
| `GET /api/v1/staff/locales/keys` | `` `${ENDPOINT}/keys` `` in `views/locales.ts` |

**Duplicates — a screen exists, under the other spelling:**

| Surface | The one the panel uses |
|---|---|
| `GET /api/v1/staff/sessions`, `DELETE /api/v1/staff/sessions/{id}` | `/api/v1/staff/security/sessions…`, read by `/security` |
| `POST /api/v1/staff/affiliates/{id}/payout` | `/payouts`, called by the payout dialog |
| `POST /api/v1/staff/invoices/{id}/void-reason` | `POST /void` with the reason in the body |
| `POST /api/v1/staff/tickets/{id}/attachments` | the ticket screen uploads through the portal path for both audiences |

Retiring the duplicate session routes is a backend change, so it is recorded rather than
done here.

**Genuine gaps, recorded because the screen belongs to another owner:**

| Surface | What an operator cannot do |
|---|---|
| `GET /api/v1/staff/upgrades` | see the pending-upgrade queue. `/upgrades` edits the graph (`catalog:write`); a holder of `service:read:all` sees nothing. |
| `GET /api/v1/portal/affiliate` | a customer who is an affiliate cannot see their referrals or commission |
| `GET /api/v1/staff/affiliates/payouts` | see the all-affiliates payout report; `/affiliates` reads one affiliate at a time |
| `GET /api/v1/staff/credit-notes/{id}/pdf` | download the credit note they just issued from `/invoices/:id` |
| `GET /api/v1/portal/credit-notes/{id}/pdf` | download the credit note they were sent |

| Surface | Verdict |
|---|---|
| `POST /api/v1/portal/cart/items` | **the headline gap** — see the purchase funnel above |
| `POST /api/v1/portal/invoices/{id}/pay-with-credit` | a customer with credit cannot spend it on an invoice from the portal |
| `POST /api/v1/portal/services/{id}/cancellation` | the portal cancels through the older `/cancel`; the newer request row the staff queue reads is API-only |
| `POST /api/v1/portal/services/{id}/upgrade` | a customer cannot request an upgrade from the portal |
| `PUT /api/v1/portal/services/{id}/config` | a customer cannot edit a service's config options |
| `PUT /api/v1/portal/services/{id}/metadata` | a customer cannot edit the properties marked `customer_visible` |
| `POST /api/v1/staff/services/{id}/hooks/{extension}/{action}` | an extension can contribute a service action; no screen renders it |
| `POST /api/v1/portal/services/{id}/hooks/{extension}/{action}` | same, for the customer-facing action |
| `POST /api/v1/staff/affiliates/{id}/payout` | duplicate of `/payouts` |
| `POST /api/v1/staff/invoices/{id}/void-reason` | duplicate of `POST /void` |
| `POST /api/v1/staff/tickets/{id}/attachments` | duplicate of the portal upload path |

### Frontend calls with no served route (0)

None. Every path a view requests is served — which is what `make openapi-check` plus the
`every API path a view requests is one the server serves` test in `web/test/routes.test.mjs`
keep true.

### Screens with no way in (3)

| Screen | Why it has no link |
|---|---|
| `/reset-password` | reached from the emailed reset link, which carries the token |
| `/verify` | reached from the emailed verification link, which carries the token |
| `/catalog/:id` | the retired catalog URL; kept so old links and bookmarks work, superseded by `/catalog-management/:id` |

### Navigation entries with no route, permission gates, navigation permissions (0 each)

None. A menu entry whose route is undeclared would be silently dropped by `hasRoute`; a gate
on an undeclared permission would refuse every operator; a menu entry filtered by an
undeclared permission would be hidden from every role. All three are now asserted statically
in `web/test/routes.test.mjs` and in the sweep.

## The browser half: every route must render

The named-screen harness (`scripts/e2e-ui.mjs`) asserts 37 screens in depth — content,
absences, a dialog that opens and closes. It cannot see a route nobody remembered to add to
it. `scripts/e2e-routes.mjs` closes that:

* it reads `web/src/routes.ts` (the same table the router and the navigation are generated
  from) and visits **every** entry, with the seeded record ids substituted for `:params`;
* three phases — public as a stranger, staff as an administrator, customer as the seeded
  customer;
* a route fails when it shows the router's "Page not found", shows "not available in this
  build" (a missing chunk), redirects away from its own path, throws, logs a console error, or
  renders nothing;
* a record-level "not found" is tolerated only for routes whose record was not seeded — that
  is an honest answer for an id that does not exist, and treating it as a dead route would
  make the pass meaningless on an unseeded database.

This turns the class of dead link that is invisible to a link check into a failing test: the
link resolves, the route is declared, and the screen still cannot be opened.

### A placeholder is a status, not a defect

The screens land in parallel, so a declared route whose view is still a placeholder must be
runnable without failing the harness — otherwise the harness is red for work nobody has
finished and stops being run. The sweep therefore reports three outcomes, the same three the
named-screen harness uses:

| Outcome | What it means | Fails the run? |
|---|---|---|
| `ok` | the route rendered with a heading, content and no error | no |
| `gap` (not built yet) | the route is declared and the router found it; the view says in its own words that it is a placeholder ("Not built yet", "not available in this build") | no — unless `--strict-placeholders` |
| `FAIL` | the router answered "Page not found", the route redirected away from itself, it threw, it logged a console error, or it rendered nothing | yes |

The closing line states which of the two it proved — `route sweep: no dead links (74 ok, 1
declared but not built yet)` rather than `every route renders` — because a harness that rounds
a placeholder up to a pass is how a missing screen survives to a bug report.

`E2E_STRICT_PLACEHOLDERS=1 bash scripts/browser-e2e.sh` (or `--strict-placeholders` on the
sweep directly) turns the tolerance off. That is the mode a release check runs: once every
screen exists, "not built yet" must fail.

`/ticket-fields` is the current example. It is routed, it is in the navigation, and its view is
a placeholder card; the named-screen harness now classifies it as a gap instead of counting it
as present, and the sweep reports it by name.

### The lock leaks through the browser (fixed)

`browser-e2e.sh` takes an exclusive `flock` so two runs cannot interleave their seeds. The
descriptor was inherited by the browser the harness spawns, so a run that was killed left an
orphaned Chromium holding the lock: every later run then failed with "another run holds the
lock" while no script was running. The descriptor is now closed for the server and for both
harness invocations (`9>&-`), and the failure message names the diagnostic command so a stale
holder is findable instead of mysterious.

## Limitations

* **Group granularity.** A route is "covered" when a view references its surface. A view that
  reads `/api/v1/staff/catalog/products` covers every product sub-route; a new sub-route with
  no call will not be flagged by the read check (the write check may catch it).
* **Static analysis cannot see every path.** A path assembled at runtime from server data
  (an attachment's `url`, an OAuth provider's `start_url`) is invisible to the diff; those are
  recorded above with the reason.
* **Method resolution is best-effort.** A call through a helper or a variable may not resolve
  to a method; where the path literal exists the route is treated as covered. The
  `write-without-ui` list is therefore the routes with *no* evidence, which is why it stays
  short enough to review.
* **The sweep needs Go; the static tests need only Node.** `web/test/routes.test.mjs` asserts
  the invariants that do not need the served table (navigation, gates, documented paths). CI
  should run `scripts/reachability.sh --check` for the rest.

## Adding a screen or a route

1. Add the route to `web/src/routes.ts` and the view under `web/src/views/`.
2. Add the navigation entry to `web/src/ui/layout.ts` — `hasRoute` will not show a route the
   table does not declare, and the sweep will report a screen with no way in.
3. If the screen is reached only by a link (a detail page, an emailed token), make sure the
   link is a literal the sweep can see, or record the route in `UNLINKED_ROUTES` in
   `web/test/routes.test.mjs` with the reason.
4. If the route is API-only, record it in `RECORDED` in `scripts/reachability.sh` with the
   reason. `--check` fails otherwise, which is the point: the decision is made once, in the
   diff, instead of being rediscovered by a user.
