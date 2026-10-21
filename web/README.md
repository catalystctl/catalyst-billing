# The frontend

A single-page TypeScript application, bundled by esbuild, embedded in the Go binary by
`web/web.go`. There is no framework and no runtime dependency: the entire bundle is ~70 KiB
before gzip.

```
web/
  build.mjs          esbuild + content hashing + index.html rewriting
  src/
    index.html       the shell template ({{STYLE}} / {{SCRIPT}} are replaced at build time)
    main.ts          entry point: styles + bootstrap
    app.ts           session, shell, router, live feed, ctx construction
    lib/             api client, router, dom, format, icons, theme, session, types
    ui/              kit (primitives), table, dialog, toast, layout, charts
    views/           one module per screen
    styles/          tokens, base, layout, components, tables, pages
  dist/              build output, committed, embedded by //go:embed
```

## Commands

```sh
cd web
npm install          # once
npm run build        # writes dist/ with hashed asset names
npm run typecheck    # tsc --noEmit
```

`make build` (Go) embeds whatever is in `web/dist`, so a frontend change must be rebuilt
before the binary is rebuilt.

## Hard rules

1. **No inline styles.** The Content-Security-Policy is `style-src 'self'` with no
   `unsafe-inline`, so a `style="..."` attribute or `el.style.width = ...` is refused by the
   browser. Use a class from `styles/`, or a `data-*` attribute with a matching rule.
2. **No `innerHTML` with data.** `h()` turns strings into text nodes. The only `innerHTML` in
   the codebase is `lib/icons.ts`, whose input is a closed union of literals.
3. **No `fetch`.** Every request goes through `ctx.api`, which attaches the CSRF header and a
   required `Idempotency-Key` to mutations, and maps failures to `ApiError` with a stable
   `code`.
4. **Money is never a float.** Render amounts with `moneyEl(amount)` (from the API's
   `{minor, currency, scale}`) and parse input with `amountFromInput()`. Never `Number(x) *
   100`.
5. **Permissions gate the UI, the server gates the data.** Use `ctx.can("invoice:write:all")`
   to hide an action; never assume hiding is enforcement.
6. **Every page handles four states:** loading (skeleton), empty (with a next step), error
   (retry), and loaded. `resource()` and `createTable()` do most of it for you.
7. **No dead ends.** A page that shows a record links to the records it references.
8. **Mobile is a layout, not a squeeze.** Mark the identifying column `primary: true` in a
   table; the CSS turns the row into a card below 780px and labels every other cell.

## A page module

```ts
import { h } from "../lib/dom";
import type { Ctx } from "../lib/types";
import { pageHead, card } from "../ui/kit";

export const meta = { title: "Customers", crumb: "Customers" };

export default async function customersPage(ctx: Ctx): Promise<Node> {
  return h("div", { class: "page" }, pageHead({ title: "Customers" }), card({ body: "…" }));
}
```

`meta.title` sets the document title; `meta.crumb` overrides the last breadcrumb segment.
The router resolves `/customers/:id` into `ctx.route.params.id`.

## `ctx`

| member | meaning |
| --- | --- |
| `ctx.api` | `get/post/put/del`, `ApiError`, `invalidate` |
| `ctx.session` | `{ user, permissions: Set<string>, csrfToken }` |
| `ctx.route` | `{ path, params, query }` |
| `ctx.can(permission)` | holds a permission |
| `ctx.navigate(path)` | client-side navigation |
| `ctx.replaceQuery({k: v \| null})` | update the URL without remounting |
| `ctx.toast(title, {detail, tone, duration})` | transient receipt |
| `ctx.confirm({...})` | modal confirmation; supports `phrase` |
| `ctx.formDialog({title, body, onSubmit})` | modal form; `onSubmit` returns an error string to keep it open |
| `ctx.setTitle(t)` / `ctx.setCrumbs([...])` | document title / breadcrumbs |
| `ctx.refreshSession()` | reload identity + permissions |
| `ctx.onDispose(fn)` | cleanup on navigation (abort controllers) |
| `ctx.onLive(fn)` | subscribe to committed server events for this page's lifetime |

## The kit (`ui/kit.ts`)

Layout: `pageHead`, `card`, `sectionTitle`, `metaGrid`, `kv`, `banner`, `tabs`,
`progressBar`, `emptyState`, `errorState`, `skeletonPage`, `skeletonTable`, `spinner`,
`resource`, `mutate`.

Data display: `badge`, `statusBadge`, `moneyEl`, `avatar`, `code`, `mono`, `cellMain`,
`copyButton`.

Controls: `btn`, `iconBtn`, `field`, `input`, `textarea`, `select`, `checkboxField`,
`switchField`, `searchInput`, `formValues`, `withBusy`.

Tables: `createTable(ctx, {path, columns, query, empty, rowHref, limit, footNote, onLoaded})`
returns `{el, reload(query), rows, setLoading}`. Columns are
`{key, label, render, align, primary, width, hideSmall, className}`.

Dialogs: `ctx.confirm`, `ctx.formDialog`, or `openDialog` from `ui/dialog.ts` for a custom
body/footer.

## CSS vocabulary

Layout: `.page`, `.page-head`, `.page-title`, `.page-sub`, `.page-actions`, `.toolbar`,
`.split` (main + aside), `.split-even`, `.grid`, `.grid-2/3/4`, `.stack`, `.stack-sm`, `.row`,
`.row-wrap`, `.spacer`.

Components: `.card`, `.card-head/body/foot`, `.btn` + `--primary/--ghost/--danger/--sm/--lg/
--block`, `.badge` + tone, `.chip`, `.field`, `.input`, `.select`, `.textarea`, `.switch`,
`.segmented`, `.tabs/.tab`, `.banner` + tone, `.kv`, `.meta-grid`, `.timeline/.tl-*`,
`.money`, `.doc-*` (invoice document), `.line-*` (line editor), `.table*`, `.pager`,
`.empty`, `.error-state`, `.skeleton*`, `.code-block`, `.inline-code`.

Page-specific classes already defined in `styles/pages.css` are fair game; add new ones
there (not inline) if a screen needs a shape that does not exist.

## API shapes

The handlers are the specification. Read the Go source before inventing a field:

- `internal/httpapi/staff_read.go` — customers, invoices, services, orders, payments,
  operations, webhooks, catalog (list + detail payloads).
- `internal/httpapi/staff_write.go` — suspend/resume/terminate, holds, invoice create/issue/
  void.
- `internal/httpapi/money.go` — refunds, credit notes, credit application.
- `internal/httpapi/portal.go` — the customer portal.
- `internal/httpapi/billing_profile.go` — billing profiles.
- `internal/httpapi/approvals.go`, `apikeys.go`, `auth_lifecycle.go`, `operations.go`.
- `internal/httpapi/routes_core.go` — the route table.
- `docs/ui-gate.md` — why each endpoint exists, and the errors it can return.

Errors are `{type, title, status, detail, code}`. Branch on `code`, never on `detail`.

Common codes: `not_found`, `invalid_body`, `invalid_query`, `invalid_cursor`, `forbidden`,
`authentication_required`, `csrf_failed`, `approval_required`, `exceeds_refundable`,
`not_refundable`, `not_draft`, `not_voidable`, `not_eligible`, `hold_in_place`,
`still_owing`, `confirmation_mismatch`, `insufficient_credit`, `exceeds_invoice`,
`self_approval_refused`, `already_decided`, `scope_not_held`, `invalid_token`,
`weak_password`.

Money is always `{minor, currency, scale, display}`. Timestamps are RFC3339 strings or null.
Lists are `{data: [...], next_cursor: string|null}`.
