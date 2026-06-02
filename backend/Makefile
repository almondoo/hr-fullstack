.PHONY: build test lint vuln migrate-up tidy

# ── Build ─────────────────────────────────────────────────────────────────────
build:
	go build ./...

# ── Test (race detector; requires Docker for testcontainers) ──────────────────
test:
	go test ./... -race -timeout 600s -count=1

# ── Lint ──────────────────────────────────────────────────────────────────────
lint:
	golangci-lint run --config .golangci.yml --timeout 5m

# ── Vulnerability check ───────────────────────────────────────────────────────
vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# ── Migrations ────────────────────────────────────────────────────────────────
migrate-up:
	go run ./cmd/migrate up

# ── Module hygiene ────────────────────────────────────────────────────────────
tidy:
	go mod tidy
