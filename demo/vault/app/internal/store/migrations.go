package store

import (
	"context"
	"fmt"
)

// schema is the full set of tables. It is intentionally idempotent (IF NOT EXISTS) so
// Open can run it on every boot; this demo has no incremental-migration machinery.
const schema = `
CREATE TABLE IF NOT EXISTS users (
	id            INTEGER PRIMARY KEY AUTOINCREMENT,
	username      TEXT    NOT NULL UNIQUE,
	password_hash TEXT    NOT NULL,
	kdf_salt      BLOB    NOT NULL,
	created_at    TIMESTAMP NOT NULL
);

CREATE TABLE IF NOT EXISTS secrets (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	tag        TEXT NOT NULL DEFAULT '',
	ciphertext BLOB NOT NULL,
	expires_at TIMESTAMP,
	created_at TIMESTAMP NOT NULL,
	updated_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_secrets_updated ON secrets (updated_at DESC);

CREATE TABLE IF NOT EXISTS audit_log (
	id     INTEGER PRIMARY KEY AUTOINCREMENT,
	action TEXT NOT NULL,
	target TEXT NOT NULL DEFAULT '',
	at     TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_audit_at ON audit_log (at DESC);
`

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("store: migrate: %w", err)
	}
	return nil
}
