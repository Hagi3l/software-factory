package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Loxstomper/software-factory/internal/controlroom"
)

// cmdServe starts the control room web server (specs/control-room.md) — the human's
// read-only window into the factory and, via the wizard (later Phase 4 tasks), their only
// action surface. This is the T4.1 scaffold: it serves the base layout and embedded
// assets; the data-backed views attach in T4.2+. It runs until interrupted, then drains
// in-flight requests, mirroring `harness run`'s "Ctrl-C is a clean stop" contract.
func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8080", "address to listen on")
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := controlroom.New(controlroom.Options{Version: version, Logger: log})
	return srv.ListenAndServe(ctx, *addr)
}
