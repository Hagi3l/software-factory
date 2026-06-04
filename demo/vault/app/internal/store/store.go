// Package store is the vault's persistence layer over SQLite (pure-Go modernc driver).
//
// It is deliberately encryption-agnostic: secret values arrive as already-sealed blobs
// (the web layer holds the derived key and seals/opens them). The store's job is the SQL
// — schema, CRUD, and the append-only audit log — using parameterized queries throughout
// so it stays injection-free.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("store: not found")

// Store wraps a SQLite database handle.
type Store struct {
	db *sql.DB
}

// User is the single vault owner (this demo is single-user).
type User struct {
	ID           int64
	Username     string
	PasswordHash string // Argon2id verifier (see crypto.HashPassword)
	KDFSalt      []byte // salt for deriving the at-rest encryption key
	CreatedAt    time.Time
}

// Secret is one stored credential. Ciphertext is nonce||sealed-bytes (see crypto.Seal).
type Secret struct {
	ID         int64
	Name       string
	Tag        string
	Ciphertext []byte
	ExpiresAt  *time.Time // nil = never expires
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// AuditEntry is one append-only record of a sensitive action.
type AuditEntry struct {
	ID     int64
	Action string // e.g. "login", "reveal", "create", "update", "delete"
	Target string // secret name or "" for account-level actions
	At     time.Time
}

// Open opens (creating if needed) the SQLite database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite: serialize writers, avoids "database is locked"
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// --- users ---

// CreateUser inserts the vault owner. Returns the created row.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string, kdfSalt []byte) (User, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, kdf_salt, created_at) VALUES (?, ?, ?, ?)`,
		username, passwordHash, kdfSalt, now)
	if err != nil {
		return User{}, fmt.Errorf("store: create user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return User{}, fmt.Errorf("store: create user id: %w", err)
	}
	return User{ID: id, Username: username, PasswordHash: passwordHash, KDFSalt: kdfSalt, CreatedAt: now}, nil
}

// UserByUsername loads a user by name, or ErrNotFound.
func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	var u User
	err := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, kdf_salt, created_at FROM users WHERE username = ?`,
		username).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.KDFSalt, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("store: user by username: %w", err)
	}
	return u, nil
}

// CountUsers returns how many users exist (the UI uses 0 to mean "first-run setup").
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

// --- secrets ---

// CreateSecret inserts a sealed secret and returns it.
func (s *Store) CreateSecret(ctx context.Context, name, tag string, ciphertext []byte, expiresAt *time.Time) (Secret, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO secrets (name, tag, ciphertext, expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		name, tag, ciphertext, expiresAt, now, now)
	if err != nil {
		return Secret{}, fmt.Errorf("store: create secret: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Secret{}, fmt.Errorf("store: create secret id: %w", err)
	}
	return Secret{ID: id, Name: name, Tag: tag, Ciphertext: ciphertext, ExpiresAt: expiresAt, CreatedAt: now, UpdatedAt: now}, nil
}

// SecretByID loads one secret, or ErrNotFound.
func (s *Store) SecretByID(ctx context.Context, id int64) (Secret, error) {
	var sec Secret
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, tag, ciphertext, expires_at, created_at, updated_at FROM secrets WHERE id = ?`,
		id).Scan(&sec.ID, &sec.Name, &sec.Tag, &sec.Ciphertext, &sec.ExpiresAt, &sec.CreatedAt, &sec.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	}
	if err != nil {
		return Secret{}, fmt.Errorf("store: secret by id: %w", err)
	}
	return sec, nil
}

// ListSecrets returns secrets whose name or tag matches the (optional) query, newest
// first. An empty query lists all.
func (s *Store) ListSecrets(ctx context.Context, query string) ([]Secret, error) {
	like := "%" + query + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, tag, ciphertext, expires_at, created_at, updated_at
		   FROM secrets
		  WHERE (? = '' OR name LIKE ? OR tag LIKE ?)
		  ORDER BY updated_at DESC, id DESC`,
		query, like, like)
	if err != nil {
		return nil, fmt.Errorf("store: list secrets: %w", err)
	}
	defer rows.Close()

	var out []Secret
	for rows.Next() {
		var sec Secret
		if err := rows.Scan(&sec.ID, &sec.Name, &sec.Tag, &sec.Ciphertext, &sec.ExpiresAt, &sec.CreatedAt, &sec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: scan secret: %w", err)
		}
		out = append(out, sec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate secrets: %w", err)
	}
	return out, nil
}

// UpdateSecret replaces a secret's metadata and sealed value.
func (s *Store) UpdateSecret(ctx context.Context, id int64, name, tag string, ciphertext []byte, expiresAt *time.Time) error {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`UPDATE secrets SET name = ?, tag = ?, ciphertext = ?, expires_at = ?, updated_at = ? WHERE id = ?`,
		name, tag, ciphertext, expiresAt, now, id)
	if err != nil {
		return fmt.Errorf("store: update secret: %w", err)
	}
	return affectedOne(res)
}

// DeleteSecret removes a secret.
func (s *Store) DeleteSecret(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete secret: %w", err)
	}
	return affectedOne(res)
}

// --- audit ---

// AppendAudit records one sensitive action.
func (s *Store) AppendAudit(ctx context.Context, action, target string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (action, target, at) VALUES (?, ?, ?)`,
		action, target, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("store: append audit: %w", err)
	}
	return nil
}

// RecentAudit returns the most recent audit entries, newest first, up to limit.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, action, target, at FROM audit_log ORDER BY at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent audit: %w", err)
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Action, &e.Target, &e.At); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate audit: %w", err)
	}
	return out, nil
}

func affectedOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
