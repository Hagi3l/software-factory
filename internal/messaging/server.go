package messaging

import (
	"errors"
	"fmt"
	"os"
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

	ns, err := natsserver.NewServer(&natsserver.Options{
		ServerName: name,
		JetStream:  true,
		StoreDir:   storeDir,
		DontListen: true, // in-process only; flip to a listener for distributed deployment
		NoLog:      true,
		NoSigs:     true,
	})
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
