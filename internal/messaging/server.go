package messaging

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// ServerConfig configures the embedded NATS server.
type ServerConfig struct {
	// Name is the server name shown in logs/monitoring; defaults to "harness".
	Name string
	// StoreDir is the JetStream file-storage directory. If empty, a temporary
	// directory is created and removed on Shutdown — convenient for tests, but a
	// real deployment should set a durable path so JetStream (and thus the
	// orchestrator's crash recovery) survives restarts.
	StoreDir string
	// ClientAddr, when non-empty (host:port), makes the server ALSO listen for external
	// TCP client connections, so a separate process — `software-factory approve` / `software-factory reject`
	// — can publish to it. Empty keeps the server in-process only (DontListen), the default
	// bootstrap transport (in-process connections always work regardless). This is a
	// single-host convenience for the trusted-dev approval loop, not the distributed-NATS
	// cluster (T5.8): no clustering, just a local listener (see specs/messaging.md).
	ClientAddr string
}

// EmbeddedServer is an in-process NATS server with JetStream enabled — the
// bootstrap transport. Components connect to it in-process (no TCP) yet still speak
// NATS, so the same code runs unchanged against an external cluster later (location
// transparency, see specs/messaging.md).
type EmbeddedServer struct {
	ns          *natsserver.Server
	storeDir    string
	removeStore bool
}

// NewEmbeddedServer starts an in-process NATS+JetStream server and blocks until it
// is ready for connections.
func NewEmbeddedServer(cfg ServerConfig) (*EmbeddedServer, error) {
	name := cfg.Name
	if name == "" {
		name = "harness"
	}

	storeDir := cfg.StoreDir
	removeStore := false
	if storeDir == "" {
		d, err := os.MkdirTemp("", "harness-nats-")
		if err != nil {
			return nil, fmt.Errorf("messaging: create jetstream store dir: %w", err)
		}
		storeDir = d
		removeStore = true
	}

	nopts := &natsserver.Options{
		ServerName: name,
		JetStream:  true,
		StoreDir:   storeDir,
		DontListen: true, // in-process only by default; ClientAddr flips on a TCP listener
		NoLog:      true,
		NoSigs:     true,
	}
	// A configured ClientAddr opens a TCP listener so a separate process can connect, in
	// addition to the always-available in-process connection. Parse it up front so a bad
	// address fails loudly here rather than leaving the server silently in-process only.
	if cfg.ClientAddr != "" {
		host, portStr, err := net.SplitHostPort(cfg.ClientAddr)
		if err != nil {
			if removeStore {
				_ = os.RemoveAll(storeDir)
			}
			return nil, fmt.Errorf("messaging: parse client addr %q: %w", cfg.ClientAddr, err)
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			if removeStore {
				_ = os.RemoveAll(storeDir)
			}
			return nil, fmt.Errorf("messaging: client addr %q has non-numeric port: %w", cfg.ClientAddr, err)
		}
		nopts.DontListen = false
		nopts.Host = host
		nopts.Port = port
	}

	ns, err := natsserver.NewServer(nopts)
	if err != nil {
		if removeStore {
			_ = os.RemoveAll(storeDir)
		}
		return nil, fmt.Errorf("messaging: new nats server: %w", err)
	}

	ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		if removeStore {
			_ = os.RemoveAll(storeDir)
		}
		return nil, errors.New("messaging: nats server not ready within timeout")
	}

	return &EmbeddedServer{ns: ns, storeDir: storeDir, removeStore: removeStore}, nil
}

// Connect dials an EXTERNAL NATS server at url (the distributed deployment, T5.8) —
// the swap-in for the embedded in-process server when the infra overlay points
// nats.url at a cluster instead of leaving it empty. It is the location-transparency
// seam: the orchestrator and runner take the returned *nats.Conn unchanged, so the
// only thing that differs between a single-process dev run and a distributed cluster
// is whether the composition root started an EmbeddedServer or dialed here. The url is
// a standard nats URL (nats://host:port, comma-separated for a cluster); credentials,
// if any, ride nats.Option (never config — like every other secret). A failed dial is
// the common "is the cluster up / reachable?" error and is wrapped plainly.
func Connect(url string, opts ...nats.Option) (*nats.Conn, error) {
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		return nil, fmt.Errorf("messaging: connect to external nats at %s: %w", url, err)
	}
	return nc, nil
}

// Connect opens an in-process client connection to the embedded server. Callers may
// pass extra options (connection name, error handlers, etc.).
func (s *EmbeddedServer) Connect(opts ...nats.Option) (*nats.Conn, error) {
	opts = append([]nats.Option{nats.InProcessServer(s.ns)}, opts...)
	nc, err := nats.Connect("", opts...)
	if err != nil {
		return nil, fmt.Errorf("messaging: connect in-process: %w", err)
	}
	return nc, nil
}

// Shutdown stops the server and removes the store directory if NewEmbeddedServer
// created a temporary one.
func (s *EmbeddedServer) Shutdown() {
	s.ns.Shutdown()
	s.ns.WaitForShutdown()
	if s.removeStore {
		_ = os.RemoveAll(s.storeDir)
	}
}
