package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Loxstomper/harness/internal/broker"
	"github.com/Loxstomper/harness/internal/goproxy"
)

// cmdSandboxGoproxy runs the in-sandbox GOPROXY shim (T5.6). Unlike the operator-facing
// subcommands this one runs INSIDE the sandbox: the go-toolchain image's entrypoint starts
// it, and `go`'s GOPROXY points at it, so `go mod download` of a dependency the sandbox does
// not already have cached is forwarded — over the bind-mounted broker socket — to the
// runner, which fetches it from the package proxy and logs it (the one egress chokepoint).
// It bridges HTTP (what `go` speaks) to the broker's framed RPC (what the runner speaks);
// it holds no policy — the allowlist and proxy base live on the runner, so a denied fetch
// is rejected there and relayed back to `go` as a failure. See specs/security.md Control 2,
// specs/components/runner.md, internal/goproxy.
func cmdSandboxGoproxy(args []string) error {
	fs := flag.NewFlagSet("sandbox-goproxy", flag.ContinueOnError)
	brokerEndpoint := fs.String("broker", "unix:/run/harness/broker.sock",
		"runner broker endpoint as <network>:<address> (e.g. unix:/run/harness/broker.sock or vsock:2:1024)")
	addr := fs.String("addr", "127.0.0.1:8123", "loopback address to serve the GOPROXY on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	network, address, err := parseBrokerEndpoint(*brokerEndpoint)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	client := broker.NewClient(network, address)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           goproxy.Handler(client, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// Cancel the serve ctx -> Shutdown the server, draining in-flight fetches. The sandbox is
	// reaped wholesale at teardown, so this is a best-effort clean stop, not load-bearing.
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()

	log.Info("sandbox-goproxy: serving", "addr", *addr, "broker", *brokerEndpoint)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("sandbox-goproxy: serve: %w", err)
	}
	return nil
}

// parseBrokerEndpoint splits a "<network>:<address>" broker endpoint into its parts on the
// FIRST colon only, so a vsock address ("vsock:2:1024") or a unix path keeps its remainder
// intact. It mirrors sandbox.Endpoint{Network,Address}, the value the runner threads to the
// sandbox; the shim is the one in-sandbox dialer that must reconstruct it from a flag.
func parseBrokerEndpoint(s string) (network, address string, err error) {
	i := strings.IndexByte(s, ':')
	if i <= 0 || i == len(s)-1 {
		return "", "", fmt.Errorf("sandbox-goproxy: broker endpoint %q must be <network>:<address>", s)
	}
	network, address = s[:i], s[i+1:]
	if network != "unix" && network != "vsock" {
		return "", "", fmt.Errorf("sandbox-goproxy: broker network %q must be unix or vsock", network)
	}
	return network, address, nil
}
