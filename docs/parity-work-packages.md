# Parallel work packages for Paymenter parity

The backlog in `docs/paymenter-parity.md` is 277 items. It is built in parallel by a
team, and this file is the contract that makes that safe: which paths each package owns,
which shared files it must not touch, and how it proves its work.

## The rule that makes parallelism work

Three files are **shared** and are edited only by the integrator, never by a package:

| File | Why |
|---|---|
| `internal/authn/authz.go` | The permission set is a single closed list. A package that needs a permission asks for it in its report; the integrator adds it. |
| `web/src/routes.ts` | The route table is one array. A package adds its screen by appending inside its own block, marked with a comment. |
| `web/src/ui/layout.ts` | The navigation is one function. Same rule: append inside your own group. |
| `internal/store/storetest/storetest.go` | The truncate list must classify every table. A package that adds a table appends it here. |

Everything else is owned. A package that needs a change in another package's file sends a
message instead of editing it.

## Registries already in place

- **Routes**: `RegisterRoutes("NN-name", fn)` from an `init` in the feature's own file.
  Names sort deterministically, so the route table and the OpenAPI document are stable.
- **Descriptions**: `openapi.RegisterDescriptions(fn)` from an `init` in a
  `descriptions_<domain>.go` file in `internal/openapi/`. Every route needs an entry; the
  parity test fails otherwise.
- **Frontend screens**: append to `routes` in `web/src/routes.ts`.
- **Migrations**: the next free number. Check `ls internal/store/migrations` before you
  write; do not reuse a number.

## The packages

| # | Package | Owns | Backlog IDs |
|---|---|---|---|
| P1 | Commerce core | `internal/commerce/`, `internal/httpapi/{catalog_commerce,tax,currency,coupon,cart}.go` | CPN-*, TAX-*, CUR-*, CT-01…CT-06, SF-13…SF-18, CAT-01, CAT-03 |
| P2 | Customer purchase path | `internal/httpapi/{checkout_customer,register}.go`, `internal/register/`, `internal/cart/` | IDN-01, IDN-03, IDN-06, IDN-19, IDN-21, CT-06…CT-09, SF-20, SF-21 |
| P3 | Invoice documents | `internal/invoicepdf/`, `internal/httpapi/{invoice_pdf,proforma}.go` | INV-01, INV-03, INV-04, INV-07, INV-08, INV-10…INV-12, INV-14, INV-16, INV-17 |
| P4 | Support tickets | `internal/tickets/`, `internal/httpapi/tickets*.go` | TKT-01…TKT-10, TKT-12…TKT-14, ADM-27 |
| P5 | Notifications | `internal/notify/` (extend), `internal/httpapi/notifications*.go` | NTF-01…NTF-09, NTF-11, NTF-14 |
| P6 | Service lifecycle | `internal/service/`, `internal/httpapi/{cancellations,upgrades,config}.go` | CAN-*, UPG-*, SVC-02…SVC-10, SF-07…SF-09, OPS-02, OPS-05 |
| P7 | Credits and pay flow | `internal/credit/`, `internal/httpapi/{credits,payflow}.go` | CRD-02…CRD-08, CRD-10, PAY-17…PAY-19, PAY-13, PAY-14 |
| P8 | Operator surface | `internal/settings/`, `internal/httpapi/{settings,logs,cron}.go` | ADM-01, ADM-02, ADM-05…ADM-09, ADM-13, ADM-16…ADM-22, ADM-24, ADM-25, ADM-28, ADM-29, ADM-34, OPS-08, OPS-09, OPS-15, OPS-16 |
| P9 | Identity depth | `internal/authn/` (own files), `internal/httpapi/{sessions,impersonate,apikeys_ext}.go` | IDN-12, IDN-13, IDN-14, IDN-15, IDN-17, ADM-30 |
| P10 | Frontend commerce | `web/src/views/{cart,checkout,register,coupons,tax-rates,currencies,config-options,system-settings,announcements,logs,cancellations,credits}.ts` | the UI half of every package above |
| P11 | Frontend support/account | `web/src/views/{tickets,ticket,ticket-new,support-queue,portal-credits,portal-security,portal-notifications,portal-payment-methods}.ts` | the UI half of P4, P5, P9 |
| P12 | Growth and platform | `internal/announcements/`, `internal/httpapi/{announcements,dashboard}.go` | GRW-01, ADM-01 (revenue widgets), I18N-07 |

## The never-regress list

Every package must leave these true, because they are what makes the panel better than
Paymenter rather than merely equal to it:

1. Money is integer minor units and exact rational rates. No float anywhere in a money path.
2. An issued invoice is immutable by database trigger. Corrections are credit notes.
3. Payment ingestion is idempotent on event id and provider payment id.
4. Provisioning is a queue with fencing, leases and reconciliation.
5. The audit log is append-only and written in the transaction that made the change.
6. Dual approval is enforced by a `CHECK`, not by a service check.
7. A customer-scoped query is scoped in SQL, so identifier substitution returns 404.
8. Every route is a table entry, so permission coverage and OpenAPI parity are tests.
9. Every error is `application/problem+json` with a stable `code`.
10. A mutation takes an `Idempotency-Key`; a money mutation requires one.

## Definition of done for a package

- `go build ./...` and `go vet ./...` clean.
- `gofmt -l` empty.
- New behaviour has a test that runs against the real database (`storetest.DB(t)`).
- Every new route has an OpenAPI description; `go test ./internal/openapi/` passes.
- `go test ./internal/httpapi/` passes, including the permission-coverage and
  problem+json tests — those are what prove the new routes are guarded.
- Frontend packages: `cd web && npx tsc --noEmit` and `npm run build` clean, and the new
  screen renders in `make e2e-ui` if it is reachable.
- The package's own report says what it built, what it did not, and what the integrator
  must do (a permission to add, a route to wire, a nav entry to place).
