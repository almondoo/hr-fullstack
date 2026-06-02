// Package migrations embeds all goose SQL migration files so the binary is
// fully self-contained — no external migration directory is required at
// runtime or in containers.
//
// Usage:
//
//	goose.SetBaseFS(migrations.FS)
//	goose.Up(db, ".")
package migrations

import "embed"

// FS holds all *.sql files under db/migrations/ as an embedded file system.
// The path passed to goose must match the directory structure inside the FS.
//
//go:embed migrations/*.sql
var FS embed.FS
