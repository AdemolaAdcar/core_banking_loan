# Party/CIF service — build, test, and migration targets.
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
DATABASE_URL ?= postgres://party:party@localhost:5432/party?sslmode=disable
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
# Postgres and Kafka containers via testcontainers-go, see
# internal/integration/integration_test.go.
test-integration:
	go test -tags=integration ./internal/integration/... -v

# Convenience target: a local Postgres for manual migrate-up/down
# testing outside the integration test's disposable containers.
migrate-db-up:
	docker run -d --name party-migrate-db \
		-e POSTGRES_USER=party -e POSTGRES_PASSWORD=party -e POSTGRES_DB=party \
		-p 5432:5432 postgres:16-alpine

migrate-db-down:
	docker rm -f party-migrate-db

migrate-up:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" up

migrate-down:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" down 1

migrate-version:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" version

# Usage: make migrate-force VERSION=1
migrate-force:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" force $(VERSION)
