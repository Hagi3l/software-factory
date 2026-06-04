package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if n, err := st.CountUsers(ctx); err != nil || n != 0 {
		t.Fatalf("count=%d err=%v, want 0/nil", n, err)
	}
	if _, err := st.CreateUser(ctx, "owner", "hash", []byte("salt")); err != nil {
		t.Fatalf("create user: %v", err)
	}
	u, err := st.UserByUsername(ctx, "owner")
	if err != nil {
		t.Fatalf("user by username: %v", err)
	}
	if u.PasswordHash != "hash" || string(u.KDFSalt) != "salt" {
		t.Fatalf("user fields not persisted: %+v", u)
	}
	if _, err := st.UserByUsername(ctx, "nobody"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSecretCRUDAndSearch(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	a, err := st.CreateSecret(ctx, "aws-key", "cloud", []byte("ctA"), nil)
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	if _, err := st.CreateSecret(ctx, "github-token", "vcs", []byte("ctB"), nil); err != nil {
		t.Fatalf("create b: %v", err)
	}

	all, err := st.ListSecrets(ctx, "")
	if err != nil || len(all) != 2 {
		t.Fatalf("list all = %d err=%v, want 2", len(all), err)
	}

	// search by name and by tag
	byName, _ := st.ListSecrets(ctx, "aws")
	if len(byName) != 1 || byName[0].Name != "aws-key" {
		t.Fatalf("search aws = %+v", byName)
	}
	byTag, _ := st.ListSecrets(ctx, "vcs")
	if len(byTag) != 1 || byTag[0].Name != "github-token" {
		t.Fatalf("search vcs = %+v", byTag)
	}

	// update
	if err := st.UpdateSecret(ctx, a.ID, "aws-key", "cloud", []byte("ctA2"), nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err := st.SecretByID(ctx, a.ID)
	if err != nil || string(got.Ciphertext) != "ctA2" {
		t.Fatalf("after update = %+v err=%v", got, err)
	}

	// delete
	if err := st.DeleteSecret(ctx, a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.SecretByID(ctx, a.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
	if err := st.DeleteSecret(ctx, a.ID); err != ErrNotFound {
		t.Fatalf("delete missing want ErrNotFound, got %v", err)
	}
}

func TestSecretExpiry(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	exp := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	s, err := st.CreateSecret(ctx, "temp", "", []byte("ct"), &exp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.SecretByID(ctx, s.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ExpiresAt == nil || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry not persisted: %v want %v", got.ExpiresAt, exp)
	}
}

func TestAudit(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	for _, a := range []string{"login", "create", "reveal"} {
		if err := st.AppendAudit(ctx, a, "x"); err != nil {
			t.Fatalf("append %s: %v", a, err)
		}
	}
	entries, err := st.RecentAudit(ctx, 10)
	if err != nil || len(entries) != 3 {
		t.Fatalf("recent = %d err=%v, want 3", len(entries), err)
	}
	if entries[0].Action != "reveal" {
		t.Fatalf("newest first expected reveal, got %q", entries[0].Action)
	}
}
