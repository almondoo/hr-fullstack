.PHONY: build test lint vuln migrate-up tidy smoke

# ── Build ─────────────────────────────────────────────────────────────────────
build:
	go build ./...

# ── Test (race detector; requires Docker for testcontainers) ──────────────────
# -p 1 runs packages sequentially, ensuring at most one testcontainers
# PostgreSQL container exists at a time.  See internal/platform/testdb.
test:
	go test ./... -race -timeout 600s -count=1 -p 1

# ── Lint ──────────────────────────────────────────────────────────────────────
lint:
	golangci-lint run --config .golangci.yml --timeout 5m

# ── Vulnerability check ───────────────────────────────────────────────────────
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# ── Migrations ────────────────────────────────────────────────────────────────
migrate-up:
	go run ./cmd/migrate up

# ── Smoke test (requires compose stack up) ────────────────────────────────────
# Run after `docker compose up -d`. Default port 18080 (host-side override).
smoke:
	../scripts/smoke.sh http://localhost:18080

# ── Module hygiene ────────────────────────────────────────────────────────────
tidy:
	go mod tidy
