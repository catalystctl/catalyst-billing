# The billing panel: one binary, a dedicated PostgreSQL instance, and tests
# that run against a real database.

COMPOSE ?= podman-compose

.PHONY: help db-up db-down db-reset db-logs db-psql db-psql-app db-wait migrate test test-race vet fmt build clean

help:
	@echo "db-up      start the dedicated Postgres (compose.yaml, port 5442)"
	@echo "db-down    stop it; the named volume keeps the data"
	@echo "db-reset   destroy the volume and recreate an empty cluster"
	@echo "db-psql    psql as the owner role (can DDL)"
	@echo "db-psql-app  psql as the runtime role (cannot DDL)"
	@echo "migrate    apply pending migrations"
	@echo "test       go test ./... against the real database"
	@echo "test-race  same, with the race detector"

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

db-psql-app:
	PGPASSWORD=$(APP_ROLE_PASSWORD) psql -h 127.0.0.1 -p $(POSTGRES_PORT) \
		-U billing_app -d $(POSTGRES_DB)

migrate:
	go run ./cmd/billing migrate

build:
	go build -o bin/billing ./cmd/billing

vet:
	go vet ./...

fmt:
	gofmt -l -w $$(git ls-files '*.go')

test:
	go test ./... -count=1

test-race:
	go test ./... -count=1 -race

clean:
	rm -rf bin
