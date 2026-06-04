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

	"github.com/harness-demo/vault/internal/store"
	"github.com/harness-demo/vault/internal/web"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	addr := envOr("VAULT_ADDR", "127.0.0.1:8000")
	dbPath := envOr("VAULT_DB", "vault.db")

	st, err := store.Open(context.Background(), dbPath)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = st.Close() }()

	srv := &http.Server{
		Addr:              addr,
		Handler:           web.New(st),
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
