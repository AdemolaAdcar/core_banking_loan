# Loan Account Subledger — build, test, and migration targets.
#
# DATABASE_URL defaults to the local-dev database this Makefile assumes
# you already have running; override it to point at any other Postgres
# instance, including CI's service container. Note this default connects
# as the `las` superuser for running migrations -- the application
# itself (cmd/las-service) must connect as the more restricted `las_app`
# role the migration creates; see cmd/las-service/main.go's package doc
# comment.

MIGRATE ?= migrate
DATABASE_URL ?= postgres://las:las@localhost:5432/las?sslmode=disable
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
# Postgres container via testcontainers-go. Not yet added in this
# increment -- see PR_DESCRIPTION.md's follow-up section, same rhythm as
# every prior service's integration-test phase.
test-integration:
	go test -tags=integration ./internal/integration/... -v

migrate-db-up:
	docker run -d --name las-migrate-db \
		-e POSTGRES_USER=las -e POSTGRES_PASSWORD=las -e POSTGRES_DB=las \
		-p 5432:5432 postgres:16-alpine

migrate-db-down:
	docker rm -f las-migrate-db

migrate-up:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" up

migrate-down:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" down 1

migrate-version:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" version

# Usage: make migrate-force VERSION=1
migrate-force:
	$(MIGRATE) -path $(MIGRATIONS_PATH) -database "$(DATABASE_URL)" force $(VERSION)
