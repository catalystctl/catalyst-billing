#!/usr/bin/env bash
#
# Reachability: what the panel serves that nobody can open, and what it opens that nobody can
# find.
#
# The API and the web app are two route tables that are written by different people in
# different languages. Nothing in either one forces the other to keep up. The result is the
# defect this sweep exists to find, and it has three shapes:
#
#   1. an API surface with no screen — a feature that is served, tested and documented, and
#      that no operator can reach;
#   2. a screen with no way in — a route in web/src/routes.ts that no navigation entry, link or
#      redirect points at, so it is reachable only by somebody who already knows the URL;
#   3. a dead end — a link or a navigation entry that points at a route that does not exist, a
#      screen whose permission gate names a permission this build never declares (so it can
#      never be satisfied), or a view that calls an endpoint nothing serves.
#
# The method is a diff, not a review. The served route table is generated from the router
# itself (`httpapi.DescribeRoutes`, the same projection the OpenAPI document is built from),
# and the frontend's use of it is read out of the TypeScript source. Both sides are data, so
# the diff can be re-run after every change and in CI, which is the difference between an
# audit and a one-off.
#
# Usage:
#   scripts/reachability.sh            # print the report
#   scripts/reachability.sh --check    # exit 1 if a finding is not recorded below
#   scripts/reachability.sh --json     # machine-readable findings
#
# `--check` is the mode CI runs. A finding that is not in RECORDED is either a new gap or a
# stale record, and both need a human decision; a finding that *is* recorded is printed with
# its reason so the record stays honest rather than becoming a list nobody reads.
#
# Requires: go (to build the route table), python3. No database: `DescribeRoutes` builds the
# table from the handlers, not from a connection.
set -euo pipefail
cd "$(dirname "$0")/.."

MODE="report"
for a in "$@"; do
  case "$a" in
    --check) MODE="check" ;;
    --json) MODE="json" ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "reachability: unknown argument $a" >&2; exit 2 ;;
  esac
done

WORK="$(mktemp -d "$(pwd)/reachability-tmp.XXXXXX")"
# EXIT alone misses a SIGINT/SIGTERM, and a leftover package under the module root would be
# picked up by the next `go build ./...`.
trap 'rm -rf "$WORK"' EXIT INT TERM

# The table is dumped by a program inside the module, because internal/httpapi cannot be
# imported from outside it. The directory has no leading dot: the go tool ignores those, and a
# silently empty route table would make this sweep pass by finding nothing.
cat > "$WORK/main.go" <<'GO'
package main

import (
	"fmt"

	"github.com/catalyst/billing/internal/authn"
	"github.com/catalyst/billing/internal/httpapi"
)

func main() {
	for _, r := range httpapi.DescribeRoutes() {
		fmt.Printf("ROUTE\t%s\t%s\t%s\t%s\t%t\n", r.Method, r.Pattern, r.Access, r.Permission, r.StaffOnly)
	}
	for _, p := range authn.AllPermissions() {
		fmt.Printf("PERM\t%s\n", p)
	}
}
GO

go run "./$(basename "$WORK")" > "$WORK/table.tsv" || {
  echo "reachability: could not build the route table" >&2
  exit 1
}

python3 - "$WORK/table.tsv" "$MODE" <<'PY'
import json
import os
import re
import sys

table_path, mode = sys.argv[1], sys.argv[2]
root = os.getcwd()
web = os.path.join(root, "web", "src")

# --------------------------------------------------------------- the two tables --

routes, permissions = [], []
for line in open(table_path):
    parts = line.rstrip("\n").split("\t")
    if parts[0] == "ROUTE":
        routes.append(dict(method=parts[1], pattern=parts[2], access=parts[3],
                           permission=parts[4], staff=parts[5] == "true"))
    elif parts[0] == "PERM":
        permissions.append(parts[1])

if len(routes) < 50:
    print(f"reachability: the route table has {len(routes)} entries; it did not build", file=sys.stderr)
    sys.exit(2)


def read_tree(directory, extensions=(".ts",)):
    out = []
    for base, _, names in os.walk(directory):
        for name in sorted(names):
            if name.endswith(extensions):
                path = os.path.join(base, name)
                with open(path) as handle:
                    out.append((os.path.relpath(path, root), handle.read()))
    return out


files = read_tree(web)
by_path = dict(files)


def need(rel):
    if rel not in by_path:
        print(f"reachability: {rel} is missing; the sweep needs updating", file=sys.stderr)
        sys.exit(2)
    return by_path[rel]


# Constants are per file, not global: four views declare `const ENDPOINT = "/api/v1/..."`, and
# one shared map made every `${ENDPOINT}` resolve to whichever file was read last. That turned
# a real endpoint into a phantom one, and a sweep that invents findings stops being trusted.
file_consts = {}
for path, body in files:
    found = {}
    for m in re.finditer(r'(?:export\s+)?const\s+([A-Z][A-Z0-9_]*)\s*(?::\s*string\s*)?=\s*[`"\']([^`"\']*)[`"\']', body):
        found[m.group(1)] = m.group(2)
    file_consts[path] = found


def norm(text, consts):
    """A path literal reduced to a pattern: query dropped, interpolation → {x}."""
    text = text.split("?")[0].split("#")[0]
    text = re.sub(r'\$\{([A-Za-z_][A-Za-z0-9_]*)\}', lambda m: consts.get(m.group(1), "{x}"), text)
    text = re.sub(r'\$\{[^}]*\}', "{x}", text)
    return text


def segments(path):
    return [s for s in path.split("/") if s]


def matches(pattern, path):
    """True when a concrete path is an instance of a route pattern (`{id}` or `:id`).

    A parameter matches anything, on either side: a view that dispatches
    `/staff/services/${id}/${action}` covers `/resume` as surely as `/suspend`, and the sweep
    cannot know which. What a wildcard may *not* do is stand alone: app.ts builds a breadcrumb
    href as `` `/${segment}` ``, which without the shared-literal rule matched every
    one-segment route and made the unreachable-screen check silently vacuous.
    """
    p, s = segments(pattern), segments(path)
    if len(p) != len(s):
        return False
    shared = False
    for a, b in zip(p, s):
        if a.startswith("{") or a.startswith(":") or b.startswith("{"):
            continue
        if a != b:
            return False
        shared = True
    return shared or not p


def surface(pattern):
    """The route's static prefix, which is the smallest unit a screen can be said to cover.

    `/api/v1/staff/coupons/{id}/redemptions` has the surface `/api/v1/staff/coupons`: a view
    that reads the coupon list has a screen for the group, even though the path is built from
    the row's id at click time. Judging by the full pattern would report every detail endpoint
    as invisible.
    """
    out = []
    for part in segments(pattern):
        if part.startswith("{"):
            break
        out.append(part)
    return "/" + "/".join(out)


# ------------------------------------------------------- what the frontend calls --

literal_re = re.compile(r'[`"\'](/api/v1/[^`"\']*)[`"\']')
template_re = re.compile(r'`([^`]*)`')
api_paths = {}
for path, body in files:
    consts = file_consts[path]
    for m in literal_re.finditer(body):
        api_paths.setdefault(norm(m.group(1), consts), set()).add(path)
    for m in template_re.finditer(body):
        value = norm(m.group(1), consts)
        if value.startswith("/api/v1"):
            api_paths.setdefault(value, set()).add(path)


def argument_text(body, start):
    """The text of the first argument of a call, up to its matching parenthesis."""
    depth, i = 1, start
    while i < len(body) and depth > 0:
        ch = body[i]
        if ch in "\"'`":
            quote = ch
            i += 1
            while i < len(body) and body[i] != quote:
                if body[i] == "\\":
                    i += 1
                i += 1
        elif ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
        i += 1
    return body[start:i - 1]


call_re = re.compile(r'\.(get|post|put|patch|del|delete)\s*(?:<[^(]*?>)?\s*\(')
calls = []
for path, body in files:
    consts = file_consts[path]
    for m in call_re.finditer(body):
        method = {"get": "GET", "post": "POST", "put": "PUT", "patch": "PATCH",
                  "del": "DELETE", "delete": "DELETE"}[m.group(1)]
        argument = argument_text(body, m.end())
        # A path is often chosen inside the argument — a ternary, a helper's return value, a
        # template built from a constant — so every literal in the whole argument counts.
        for lm in re.finditer(r'[`"\'](/api/v1/[^`"\']*)[`"\']', argument):
            calls.append((method, norm(lm.group(1), consts), path))
        for lm in re.finditer(r'`([^`]*)`', argument):
            value = norm(lm.group(1), consts)
            if value.startswith("/api/v1"):
                calls.append((method, value, path))
        for im in re.finditer(r'[,(]\s*([A-Z][A-Z0-9_]*)\s*[,)]', argument):
            if im.group(1) in consts:
                calls.append((method, norm(consts[im.group(1)], consts), path))
    # A raw fetch for a body the JSON client cannot carry (an image, a file).
    for m in re.finditer(r'fetch\(\s*[`"\'](/api/v1/[^`"\']*)', body):
        calls.append(("FETCH", norm(m.group(1), consts), path))


# --------------------------------------------------------------- the screens --

routes_source = need("web/src/routes.ts")
screens = [(m.group(1), m.group(2)) for m in re.finditer(
    r'route\("([^"]+)",\s*\(\)\s*=>\s*import\("\./views/([^"]+)"\)', routes_source)]
if len(screens) < 20:
    print(f"reachability: read {len(screens)} screens from routes.ts; the table moved", file=sys.stderr)
    sys.exit(2)

# Every place the frontend can send the browser: a link, a navigation call, or a template
# literal that starts with a slash (a row's href is often built that way).
reach = {}
for path, body in files:
    consts = file_consts[path]
    for m in re.finditer(r'(?:href|navigate)\s*[:=]?\s*\(?\s*[`"\'](/[^`"\']*)[`"\']', body):
        reach.setdefault(norm(m.group(1), consts), set()).add(path)
    for m in re.finditer(r'navigate\(\s*[`"\']([^`"\']+)[`"\']', body):
        reach.setdefault(norm(m.group(1), consts), set()).add(path)
    for m in re.finditer(r'`(/[^`]*)`', body):
        value = norm(m.group(1), consts)
        if value.startswith("/") and not value.startswith("/api/"):
            reach.setdefault(value, set()).add(path)

layout = need("web/src/ui/layout.ts")
layout_consts = file_consts["web/src/ui/layout.ts"]
nav = []
for m in re.finditer(r'href:\s*[`"\']([^`"\']+)[`"\']', layout):
    nav.append(norm(m.group(1), layout_consts))
# The permission a navigation entry is filtered by. A typo here hides the entry from every
# role, which makes the screen unreachable without the route table being wrong at all.
nav_permissions = {}
for m in re.finditer(r'href:\s*[`"\']([^`"\']+)[`"\']\s*,\s*icon:\s*[^,]+(?:,\s*permission:\s*"([^"]+)")?', layout):
    if m.group(2):
        nav_permissions.setdefault(m.group(2), set()).add(m.group(1))

# A view's own permission gates: ctx.can("invoice:void"), pageCtx.can(permission).
gate_permissions = {}
for path, body in files:
    for m in re.finditer(r'(?:can|has)\(\s*"([a-z][a-z0-9]*(?::[a-z0-9_]+)+)"', body):
        gate_permissions.setdefault(m.group(1), set()).add(path)

# ------------------------------------------------------------------ findings --

findings = []


def add(kind, key, detail, files=()):
    findings.append(dict(kind=kind, key=key, detail=detail, files=sorted(files)))


for route in routes:
    pattern = route["pattern"]
    surf = surface(pattern)
    exact = [p for p in api_paths if matches(pattern, p)]
    prefix = [p for p in api_paths if p == surf or p.startswith(surf + "/")]
    if not exact and not prefix:
        add("api-without-screen", f'api {route["method"]} {pattern}',
            f'{route["permission"] or route["access"]} — no view references this path or its group')
        continue
    if route["method"] == "GET":
        continue
    # A write in a group that a screen reads, but with no call the frontend can be shown to
    # make. The method is resolved from the call site where it can be; where it cannot (a
    # helper, a ternary), the route's path literal still counts, and that is the lenient
    # answer. Only the routes with neither are reported, which is what keeps this list short
    # enough to review.
    method_calls = [c for c in calls if c[0] == route["method"] and matches(pattern, c[1])]
    literals = [p for p in api_paths if matches(pattern, p)]
    if not method_calls and not literals:
        add("write-without-ui", f'write {route["method"]} {pattern}',
            f'{route["permission"] or route["access"]} — served, but no screen calls it')

for method, path, source in calls:
    if not any(matches(r["pattern"], path) for r in routes):
        add("call-without-route", f"call {method} {path}",
            f"{source} calls an endpoint the server does not serve", [source])

for pattern, view in screens:
    hits = [p for p in reach if matches(pattern, p)]
    if not hits and pattern not in nav:
        add("screen-unreachable", f"spa {pattern}",
            f"web/src/views/{view}.ts — no navigation entry, link or redirect points at it")

for href in nav:
    if not any(matches(pattern, href) for pattern, _ in screens):
        add("nav-dead", f"nav {href}", "the navigation points at a route that is not declared")

for permission, files_with in gate_permissions.items():
    if permission not in permissions:
        add("gate-unknown", f"gate {permission}",
            "a screen gates on a permission this build never declares, so no role can satisfy it",
            files_with)

for permission, hrefs in nav_permissions.items():
    if permission not in permissions:
        add("nav-permission-unknown", f"nav-permission {permission}",
            "a navigation entry is filtered by a permission this build never declares, "
            "so the entry is hidden from every role", hrefs)

# ------------------------------------------------------------------ the record --

# Every finding above is either fixed or recorded here with the reason it is deliberate. The
# reasons are the audit's real output: the diff finds the gap, and this table says whether it
# is a defect, a deliberate API-only surface, or a duplicate that should be retired.
#
# Keyed by the finding key so a route that is renamed shows up as a new, unrecorded finding
# rather than silently inheriting an old excuse.
RECORDED = {
    # -- probes and machine endpoints: no screen exists because no human is the audience --
    "api GET /healthz": "liveness probe for the orchestrator; there is nothing to show",
    "api GET /readyz": "readiness probe; the dashboard shows health, not the probe itself",
    "api GET /metrics": "Prometheus scrape; a chart of it is not the same data",
    "api POST /webhooks/{provider}": "inbound gateway callback; its outcome is on /webhooks and /logs",
    "api POST /api/v1/webhooks/mail/piping": "inbound mail webhook; the result is on /logs",
    "api GET /": "the SPA shell itself, not an API; the router answers it",
    "api GET /api/v1/me": "credential introspection for API keys; the panel has a session, not a key",
    "api GET /api/v1/me/invoices": "the API-key view of /portal/invoices; scripts only",
    "api GET /api/v1/media/{id}": "an <img src>; product images are rendered by the catalog screens",
    "api GET /oauth/authorize": "OAuth2 consent for a third-party client, not the panel",
    "api POST /oauth/authorize": "OAuth2 consent for a third-party client, not the panel",
    "api POST /oauth/token": "OAuth2 token exchange between servers; no human is present",
    "api GET /oauth/social/{slug}/start": "browser redirect; the sign-in screen follows the start_url the server returns",
    "api GET /oauth/social/{slug}/callback": "provider return URL; the server redirects back to the panel",

    # -- surfaces that are referenced through a constructed path, so the diff cannot see it --
    "api GET /api/v1/portal/tickets/meta": "reached through ticketsMetaPath(); the ticket form reads it",

    # -- duplicate surfaces: a screen exists, but it uses the other spelling --
    "api GET /api/v1/staff/sessions":
        "duplicate of /api/v1/staff/security/sessions, which /security uses; this one lists the "
        "caller's own sessions and is API-only. Retiring it is a backend change (report).",
    "api DELETE /api/v1/staff/sessions/{id}":
        "duplicate of /api/v1/staff/security/sessions/{id}, which /security uses. Retiring it is "
        "a backend change (report).",
    "write POST /api/v1/staff/affiliates/{id}/payout":
        "duplicate of /api/v1/staff/affiliates/{id}/payouts, which the payout dialog uses",
    "write POST /api/v1/staff/invoices/{id}/void-reason":
        "duplicate: /invoices/:id voids through POST /void with the reason in the body",
    "write POST /api/v1/staff/tickets/{id}/attachments":
        "the ticket screen uploads through the portal path for both audiences; this staff "
        "variant is API-only",

    # -- genuine gaps, recorded because they belong to a package that owns the area --
    "api GET /api/v1/staff/upgrades":
        "GAP: the pending-upgrade queue has no screen. /upgrades edits the upgrade graph "
        "(catalog:write); an operator holding service:read:all cannot see what customers asked "
        "to change. Recorded, not fixed here: the upgrades screen is another package's file.",
    "api GET /api/v1/portal/affiliate":
        "GAP: a customer who is an affiliate has no screen for their own referrals and "
        "commission. Recorded: a new portal screen is a product decision, not a dead-link fix.",
    "api GET /api/v1/staff/affiliates/payouts":
        "GAP: the all-affiliates payout report has no screen; /affiliates reads one affiliate's "
        "payouts at a time.",
    "api GET /api/v1/staff/refunds/{id}":
        "the refund is read from the payment it belongs to and rendered in the refund dialog; "
        "the standalone read is for API clients",
    "api GET /api/v1/portal/credit-notes/{id}/pdf":
        "GAP: a customer receives a credit note and cannot download it. The PDF endpoint exists "
        "and nothing links to it. Recorded: the invoice/credit-note screens belong to another "
        "package's files.",
    "api GET /api/v1/staff/credit-notes/{id}/pdf":
        "GAP: an operator issues a credit note from /invoices/:id and cannot download the PDF. "
        "Recorded: web/src/views/invoice.ts is another package's file.",
    "api POST /api/v1/portal/impersonation/stop":
        "GAP: no screen starts or stops an impersonation, so the audited impersonation feature "
        "is API-only. Recorded: the customer screen is another package's file.",
    "write POST /api/v1/staff/users/{id}/impersonate":
        "GAP: nothing in the panel starts an impersonation; the API exists and is audited. "
        "Recorded: the customer screen is another package's file.",
    "write POST /api/v1/portal/invoices/{id}/pay-with-credit":
        "GAP: a customer with a credit balance cannot spend it on an invoice from the portal. "
        "Recorded: the portal invoice screen is another package's file.",
    "write POST /api/v1/portal/services/{id}/cancellation":
        "GAP: the portal still cancels through the older /cancel endpoint; the newer "
        "cancellation request (the one the staff queue reads) is API-only. Recorded: which "
        "endpoint is canonical is a backend decision.",
    "write POST /api/v1/portal/services/{id}/upgrade":
        "GAP: a customer cannot request an upgrade from the portal. Recorded: the portal "
        "service screen offers cancellation only.",
    "write PUT /api/v1/portal/services/{id}/config":
        "GAP: a customer cannot edit a service's config options from the portal",
    "write PUT /api/v1/portal/services/{id}/metadata":
        "GAP: a customer cannot edit the service properties marked customer_visible",
    "write POST /api/v1/staff/services/{id}/hooks/{extension}/{action}":
        "GAP: an extension can contribute a service action and no screen renders it",
    "write POST /api/v1/portal/services/{id}/hooks/{extension}/{action}":
        "GAP: an extension can contribute a customer-facing service action and no screen renders it",
    "write PUT /api/v1/staff/invoices/{id}/client-note":
        "GAP: a client-visible note cannot be written from the panel; the invoice screen edits "
        "the invoice, not the note. Recorded: web/src/views/invoice.ts is another package's file.",
    "write POST /api/v1/staff/invoices/{id}/proforma":
        "GAP: an operator cannot issue a proforma, although the document and the setting exist. "
        "Recorded: web/src/views/invoice.ts is another package's file.",
    "write PUT /api/v1/staff/catalog/servers/{id}":
        "GAP: a catalog server can be created and listed, but not renamed or removed. Recorded: "
        "the catalog screen is another package's file.",
    "write DELETE /api/v1/staff/catalog/servers/{id}":
        "GAP: a catalog server cannot be removed from the panel. Recorded: the catalog screen "
        "is another package's file.",
    "write DELETE /api/v1/portal/social-links/{id}":
        "GAP: a linked sign-in provider can be listed and linked, but not unlinked",
    "api GET /api/v1/extensions/hooks":
        "the hook catalogue answers an extension asking what it may observe; the panel is not "
        "the caller",
    "api GET /api/v1/announcements":
        "GAP: the public announcement feed — the storefront and dashboard feed the handler "
        "documents — is read by no screen, and the announcements.enabled toggle that governs it "
        "is read by nothing. Recorded: making the feed appear is a product change, and the "
        "toggle is server-side.",
    "api GET /api/v1/catalog/category/{slug}":
        "the storefront filters in place with ?category=; a category URL is not part of the "
        "storefront's design, so the endpoint is public API only",

    # -- the purchase funnel: the largest gap in the report, recorded rather than half-fixed --
    "write POST /api/v1/portal/cart/items":
        "GAP: nothing adds a product to the cart. The storefront's Order button sends the "
        "visitor to the sign-in screen, a signed-in customer is redirected away from /products, "
        "and /cart tells them to pick a plan from the storefront. The whole purchase funnel is "
        "reachable only through the API. Recorded: it spans the router, the navigation and the "
        "storefront, which are not one file.",

    # -- screens reached without a link, deliberately --
    "spa /reset-password": "reached from the emailed reset link, which carries the token",
    "spa /verify": "reached from the emailed verification link, which carries the token",

    # -- screens reached without a link, deliberately --
    "spa /reset-password": "reached from the emailed reset link, which carries the token",
    "spa /verify": "reached from the emailed verification link, which carries the token",
    "spa /catalog/:id":
        "the retired catalog URL; it still renders the product editor so old links and "
        "bookmarks work, and /catalog-management/:id is the linked spelling",
}

# ------------------------------------------------------------------ the report --

by_key = {f["key"]: f for f in findings}
unrecorded = [f for f in findings if f["key"] not in RECORDED]
stale = [k for k in RECORDED if k not in by_key]

if mode == "json":
    print(json.dumps(dict(
        routes=len(routes), screens=len(screens), nav=len(nav),
        findings=findings,
        unrecorded=[f["key"] for f in unrecorded],
        stale=stale,
    ), indent=2))
    sys.exit(1 if unrecorded or stale else 0)

GROUPS = [
    ("api-without-screen", "API surfaces with no screen"),
    ("write-without-ui", "write endpoints with no UI affordance"),
    ("call-without-route", "frontend calls with no served route"),
    ("screen-unreachable", "screens with no way in"),
    ("nav-dead", "navigation entries with no route"),
    ("gate-unknown", "permission gates this build never declares"),
    ("nav-permission-unknown", "navigation permissions this build never declares"),
]

print(f"reachability: {len(routes)} served routes, {len(screens)} screens, {len(nav)} navigation entries")
for kind, title in GROUPS:
    group = [f for f in findings if f["kind"] == kind]
    print(f"\n{title}: {len(group)}")
    for f in group:
        reason = RECORDED.get(f["key"])
        mark = "recorded" if reason else "NEW"
        print(f"  [{mark:8}] {f['key']}")
        if reason:
            print(f"             {reason}")
        else:
            print(f"             {f['detail']}")

print(f"\n{len(findings)} findings: {len(findings) - len(unrecorded)} recorded, {len(unrecorded)} new")
if stale:
    print(f"{len(stale)} record(s) no longer match a finding — delete them so the record stays honest:")
    for key in stale:
        print(f"  stale: {key}")

if mode == "check":
    if unrecorded or stale:
        print("\nreachability: FAIL — every finding must be fixed or recorded", file=sys.stderr)
        sys.exit(1)
    print("reachability: ok — every finding is fixed or recorded with a reason")
PY
