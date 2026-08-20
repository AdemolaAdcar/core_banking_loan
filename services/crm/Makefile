# CRM service — build, test, and migration targets.
#
# Migrations use golang-migrate (https://github.com/golang-migrate/migrate),
# driven directly against migrations/*.sql — no ORM, no code generation.
# Install the CLI locally with:
#
#   go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
#
# DATABASE_URL defaults to the local-dev database this Makefile assumes
# you already have running (see migrate-db-up); override it to point at
# any other Postgres instance, including CI's service container.

MIGRATE ?= migrate
DATABASE_URL ?= postgres://crm:crm@localhost:5432/crm?sslmode=disable
MIGRATIONS_PATH := migrations

.PHONY: build vet test test-integration fmt-check \
	migrate-db-up migrate-db-down migrate-up migrate-down migrate-version migrate-force

build:
	go build ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt: the following files are not formatted:"; gofmt -l .; exit 1)

test:
	go test ./...

# Requires a working Docker daemon -- spins up its own disposable
# Postgres container via testcontainers-go, see
# internal/integration/integration_test.go. Unlike services/party's
# equivalent target, this does not also spin up Kafka -- CRM has no
# Kafka-backed outbox publisher yet (see PR_DESCRIPTION.md).
test-integration:
	go test -tags=integration ./internal/integration/... -v

# Convenience target: a local Postgres for manual migrate-up/down
# testing outside the integration test's disposable containers.
migrate-db-up:
	docker run -d --name crm-migrate-db \
		-e POSTGRES_USER=crm -e POSTGRES_PASSWORD=crm -e POSTGRES_DB=crm \
		-p 5432:5432 postgres:16-alpine

migrate-db-down:
	docker rm -f crm-migrate-db

migrate-up:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" up

migrate-down:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" down 1

migrate-version:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" version

# Usage: make migrate-force VERSION=1
migrate-force:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" force $(VERSION)
