# The billing panel: one binary, a dedicated PostgreSQL instance, and tests
# that run against a real database.

COMPOSE ?= podman-compose

.PHONY: help db-up db-down db-reset db-logs db-psql db-psql-app db-wait db-unlock migrate test test-race vet fmt build clean openapi openapi-check client env-check smoke e2e-ui web-build web-typecheck web-test

help:
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

clean:
	rm -rf bin

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
