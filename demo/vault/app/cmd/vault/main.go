// Command vault runs the secrets-vault demo web app.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/factory-demo/vault/internal/store"
	"github.com/factory-demo/vault/internal/web"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := envOr("VAULT_ADDR", "127.0.0.1:8000")
	dbPath := envOr("VAULT_DB", "vault.db")
	// The session cookie ships embeddable (SameSite=None; Secure) by default so this demo
	// app can be authenticated inside the presentation deck's cross-site iframe. Set
	// VAULT_STRICT_COOKIE to harden it back to SameSite=Strict for a standalone deployment.
	strictCookie := envOr("VAULT_STRICT_COOKIE", "") != ""

	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	srv := &http.Server{
		Addr:              addr,
		Handler:           web.New(st, web.WithStrictCookie(strictCookie)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("vault listening on http://%s (db: %s)", addr, dbPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
