# General Ledger & Posting Engine — build, test, and migration targets.
#
# Migrations use golang-migrate (https://github.com/golang-migrate/migrate),
# driven directly against migrations/*.sql — no ORM, no code generation.
# Install the CLI locally with:
#
#   go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest
#
# DATABASE_URL defaults to the local-dev database this Makefile assumes
# you already have running (see migrate-db-up); override it to point at
# any other Postgres instance, including CI's service container. Note
# this default connects as the `gl` superuser for running migrations —
# the application itself (cmd/gl-service) must connect as the more
# restricted `gl_app` role the migration creates; see
# cmd/gl-service/main.go's package doc comment.

MIGRATE ?= migrate
DATABASE_URL ?= postgres://gl:gl@localhost:5432/gl?sslmode=disable
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
# internal/integration/integration_test.go. No Kafka container -- GL has
# no Kafka-backed outbox publisher yet (see PR_DESCRIPTION.md).
test-integration:
	go test -tags=integration ./internal/integration/... -v

# Convenience target: a local Postgres for manual migrate-up/down
# testing outside the integration test's disposable containers.
migrate-db-up:
	docker run -d --name gl-migrate-db \
		-e POSTGRES_USER=gl -e POSTGRES_PASSWORD=gl -e POSTGRES_DB=gl \
		-p 5432:5432 postgres:16-alpine

migrate-db-down:
	docker rm -f gl-migrate-db

migrate-up:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" up

migrate-down:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" down 1

migrate-version:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" version

# Usage: make migrate-force VERSION=1
migrate-force:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" force $(VERSION)
