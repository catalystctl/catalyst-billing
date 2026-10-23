# The billing panel: one binary, a dedicated PostgreSQL instance, and tests
# that run against a real database.

COMPOSE ?= podman-compose

.PHONY: help dev dev-stop dev-config dev-reset seed seed-reset db-up db-ensure db-down db-reset db-logs db-psql db-psql-app db-wait db-unlock migrate test test-race vet fmt build extensions clean openapi openapi-check client env-check smoke e2e-ui money-e2e web-build web-typecheck web-test brand brand-render brand-audit brand-regress brand-verify

help:
	@echo "dev        database + schema + fixtures + demo data + a staff account, then serve (Ctrl-C stops)"
	@echo "seed       (re)build the demo dataset: customers, catalog, invoices, payments, tickets"
	@echo "seed-reset wipe the demo dataset only, leaving anything you created by hand"
	@echo "dev-reset  destroy the dev database and start over with 'make dev'"
	@echo "dev-config seed the fixture gateway/provisioner and a dev staff account (idempotent)"
	@echo "dev-stop   stop a dev server that was left running in the background"
	@echo "db-up      start the dedicated Postgres (compose.yaml, port 5442)"
	@echo "db-down    stop it; the named volume keeps the data"
	@echo "db-reset   destroy the volume and recreate an empty cluster"
	@echo "db-psql    psql as the owner role (can DDL)"
	@echo "db-psql-app  psql as the runtime role (cannot DDL)"
	@echo "migrate    apply pending migrations"
	@echo "db-unlock  clear a test lock leaked by a killed test run"
	@echo "env-check  verify .env.example documents every variable the code reads"
	@echo "test       go test ./... against the real database"
	@echo "test-race  same, with the race detector"
	@echo "openapi    regenerate api/openapi.yaml from the router"
	@echo "openapi-check  fail if api/openapi.yaml is out of date with the router"
	@echo "client     regenerate the typed client from api/openapi.yaml"
	@echo "smoke      drive the served UI in a real headless browser"
	@echo "e2e-ui     sign in through the UI and assert live data renders"
	@echo "money-e2e  drive the money documents and the small CRUD gaps in a real browser"
	@echo "brand      regenerate the logo, banners and social cards (needs Python + fontTools)"
	@echo "brand-render  rasterise the brand SVGs to PNG with Chromium"
	@echo "brand-verify  prove the served brand assets load and render in a real browser"
	@echo "web-build  rebuild the embedded frontend (needs Node; dist/ is committed)"
	@echo "web-typecheck  typecheck the frontend without emitting"
	@echo "web-test   run the frontend unit tests (money, routing)"

include .env
export

# Block until the server answers, so `make test` right after `make db-up`
# cannot fail on a cold start.
db-wait:
	@for i in $$(seq 1 60); do \
		if pg_isready -h 127.0.0.1 -p $(POSTGRES_PORT) -U $(POSTGRES_SUPERUSER) -q; then \
			echo "postgres ready on 127.0.0.1:$(POSTGRES_PORT)"; exit 0; \
		fi; \
		sleep 1; \
	done; \
	echo "postgres did not become ready" >&2; $(COMPOSE) logs --tail=50 postgres; exit 1

db-up:
	$(COMPOSE) up -d
	@$(MAKE) --no-print-directory db-wait

# Start the cluster only when it is not already answering. `podman-compose up -d` reports a
# container-name conflict when the container it wants to create already exists under a
# different project label — the database is up and healthy, and the message is noise on every
# second run of a target meant to be run repeatedly.
db-ensure:
	@if pg_isready -h 127.0.0.1 -p $(POSTGRES_PORT) -U $(POSTGRES_SUPERUSER) -q 2>/dev/null; then \
		echo "postgres already answering on 127.0.0.1:$(POSTGRES_PORT)"; \
	else \
		$(MAKE) --no-print-directory db-up; \
	fi

db-down:
	$(COMPOSE) down

db-reset:
	$(COMPOSE) down -v
	$(COMPOSE) up -d
	@$(MAKE) --no-print-directory db-wait

db-logs:
	$(COMPOSE) logs -f postgres

db-psql:
	PGPASSWORD=$(POSTGRES_SUPERUSER_PASSWORD) psql -h 127.0.0.1 -p $(POSTGRES_PORT) \
		-U $(POSTGRES_SUPERUSER) -d $(POSTGRES_DB)

# A killed test binary can leave a session-level advisory lock behind, which
# would make every later run wait. This clears it explicitly rather than having
# the harness terminate other backends heuristically.
db-unlock:
	@PGPASSWORD=$(POSTGRES_SUPERUSER_PASSWORD) psql -h 127.0.0.1 -p $(POSTGRES_PORT) \
		-U $(POSTGRES_SUPERUSER) -d $(POSTGRES_DB) -tAc \
		"select pg_terminate_backend(pid) from pg_locks where locktype='advisory' and objid=2400129460" \
		| grep -c t || true

db-psql-app:
	PGPASSWORD=$(APP_ROLE_PASSWORD) psql -h 127.0.0.1 -p $(POSTGRES_PORT) \
		-U billing_app -d $(POSTGRES_DB)

migrate:
	go run ./cmd/billing migrate

build:
	go build -o bin/billing ./cmd/billing

# Every extension binary, under the name gatewayconfig looks for by default (bin/<extension>).
# The two fixtures are what `make dev` launches; the real adapters are built too, so switching
# a provider on the integrations screen works without a second build step.
extensions:
	@for dir in extensions/*/; do \
		name="$$(basename "$$dir")"; \
		echo "building bin/$$name"; \
		go build -o "bin/$$name" "./$$dir" || exit 1; \
	done

# One command for a working local installation: the database, the schema, the binaries, the
# fixture configuration and a staff account. It runs in the foreground, so Ctrl-C stops it and
# the log is the terminal.
#
# Deliberately not part of `verify`: it starts a server and waits, which is a thing a person
# does, not a thing CI does.
dev: db-ensure migrate build extensions dev-config seed
	@bash scripts/dev.sh

# A complete demo dataset: six customers, a catalog, subscriptions, services in every state,
# invoices paid/open/overdue/void, payments, a refund, a credit note, dunning, tickets,
# notifications, webhook deliveries and an audit trail. Idempotent: it purges what it created
# (everything under @demo.test) and rebuilds it.
#
# Money is created through `billing admin`, not by INSERT, so the invoice numbers, the frozen
# document snapshots and the subledger postings are the ones the product writes; the script
# asserts the ledger still balances before reporting success.
seed: build
	@bash scripts/seed-demo.sh

# Only the demo rows go, so a customer or invoice you created by hand survives.
seed-reset:
	@./bin/billing admin purge-test-data --pattern '%@demo.test' --yes --force

# The fixture configuration on its own, for a database that is already running: the gateway and
# provisioner are pointed at the conformance fixtures and a dev staff account is created. Safe
# to run repeatedly.
dev-config: build
	@bash scripts/dev-config.sh

# Throw the dev database away and start over. Destroys the volume, so it asks for nothing and
# says so here: this is the target that deletes data.
dev-reset: db-reset
	@$(MAKE) --no-print-directory dev

# Stop the dev server this checkout started, whether `make dev` left it in a closed terminal or
# scripts/dev-ui.sh started it in the background. Matched by pid file and by the exact command
# line of this checkout's own bin/billing serve — never by port or by process name alone.
dev-stop:
	@bash scripts/dev-stop.sh

# The frontend is a separate toolchain on purpose: `go build` never needs Node, because the
# built dist/ is committed and embedded. These targets exist for when the frontend changes.
web-build:
	cd web && npm run build

web-typecheck:
	cd web && npm run typecheck

web-test:
	cd web && npm test

vet:
	go vet ./...

fmt:
	gofmt -l -w $$(git ls-files '*.go')

# The integration tests need the cluster and are skipped without it; CI sets
# BILLING_REQUIRE_DB=1 so a missing database fails instead of silently skipping.
test:
	BILLING_REQUIRE_DB=1 go test ./... -count=1

test-race:
	BILLING_REQUIRE_DB=1 go test ./... -count=1 -race

# The API document is generated from the route table, never maintained by hand.
# `openapi-check` is what CI runs: a route that is not documented, or a documented
# route that is not served, fails the build.
openapi:
	go run ./cmd/openapigen

# Fails when the binary reads an env var .env.example does not document. An undocumented
# variable works for whoever wrote it and is undiscoverable for everyone else.
env-check:
	@node scripts/env-check.mjs

openapi-check:
	go run ./cmd/openapigen -check

# A typed client generated from the document. It exists so the frontend cannot
# hand-write a type that disagrees with the server: `client-check` proves it compiles.
client: openapi
	go run ./cmd/apiclient -out ./api/client

client-check: openapi-check
	go run ./cmd/apiclient -out ./api/client -check
	go build ./api/client/...

smoke: build
	bash scripts/browser-smoke.sh

# The authenticated browser test. Separate from `smoke` because it creates accounts and needs a
# writable database, while `smoke` must run against any server. It seeds a customer, an invoice,
# a service and a portal login, then drives every screen through scripts/e2e-ui.mjs and fails on
# a console error, an error state, or a screen that renders no content.
#
# Set E2E_SCREENSHOTS=dir to keep a PNG of each screen.
e2e-ui: build
	bash scripts/browser-e2e.sh

# The money documents, clicked rather than inspected. `e2e-ui` asserts that screens render; this
# drives the affordances that move documents: the invoice and credit-note downloads on both
# audiences, the client-visible note, issuing a proforma, renaming and deleting a catalogue
# server, and unlinking a sign-in provider. It seeds its own fixtures (an issued invoice, a
# draft, a credit note, a server and a linked provider) and removes them on the way out.
#
# Set E2E_SCREENSHOTS=dir to keep a PNG of each step.
money-e2e: build
	bash scripts/browser-money-documents.sh

clean:
	rm -rf bin

# --------------------------------------------------------------- brand --
#
# The logo, banners and social cards in assets/brand are generated, not hand-drawn: they
# reuse the parent Catalyst mark, palette and typefaces, and every text run is outlined so
# no asset depends on an installed font. See assets/brand/README.md.
#
# Deliberately not part of `verify`: this needs Python with fontTools plus three woff2
# files, and it only runs when the brand changes. `brand-verify` is the part CI could run —
# it needs nothing but a served panel and Chromium, and it proves what reading the SVG
# cannot: that a browser accepts and renders it.
PYTHON ?= python3
brand:
	$(PYTHON) scripts/brand/build.py

brand-render:
	bash scripts/brand/render.sh

# The offline half of brand verification: colour and composition, straight off the PNGs.
# Runs in the same target as the browser half, because either one alone is a false green.
brand-audit:
	$(PYTHON) scripts/brand/audit.py assets/brand/png/*.png
	$(PYTHON) scripts/brand/regress.py

brand-verify:
	@$(MAKE) --no-print-directory brand-audit
	@node scripts/brand/verify.mjs
# Full verification: the same sequence CI runs, minus the network-dependent
# scanners. Run this before calling anything done.
# The browser smoke test is part of verify: the UI is only proven by a browser, and a check
# nobody runs is a check that rots.
verify: fmt-check vet openapi-check client-check test-race smoke e2e-ui
	@echo "verify: ok"

fmt-check:
	@unformatted="$$(gofmt -l . | grep -v '^$$' || true)"; \
	if [ -n "$$unformatted" ]; then echo "not gofmt'd:"; echo "$$unformatted"; exit 1; fi

# Coverage across the packages that have tests. Packages without a test file are
# excluded on purpose: `go test ./...` fails on them with "no such tool covdata",
# and listing them explicitly makes it obvious which packages are untested rather
# than hiding that behind a broken report.
# Every package that has tests. `go test ./...` would fail with "no such tool
# covdata" for packages without a test file, which makes the report useless exactly
# where it matters.
# storetest has no tests of its own, and fulfilment/payments are exercised entirely
# through the end-to-end suite, so they carry no test files to instrument.
COVER_PKGS := $(shell go list ./internal/... | grep -v -e '/storetest$$' -e '/fulfilment$$' -e '/payments$$')

cover:
	BILLING_REQUIRE_DB=1 go test $(COVER_PKGS) -count=1 -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1

# Per-package coverage, worst first: the packages at the bottom are where a missing
# test is most likely to be hiding a defect.
cover-report:
	BILLING_REQUIRE_DB=1 go test $(COVER_PKGS) -count=1 -coverprofile=coverage.out -covermode=atomic >/dev/null
	go tool cover -func=coverage.out | grep -v '^total' | awk '{print $$3, $$1}' | sort -n | head -20
