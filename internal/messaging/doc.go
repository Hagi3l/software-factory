// Package messaging is the NATS layer every factory component communicates over.
// Even when co-located in one process, components speak NATS rather than in-process
// channels, so a runner that is a goroutine today can become a separate binary on
// another host tomorrow with no code change — location transparency is a first
// principle (see specs/messaging.md).
//
// It owns three things: the subject taxonomy (so subjects are built in one place,
// never hand-typed); an embeddable in-process NATS+JetStream server (the bootstrap
// transport, swappable for an external cluster later); and the JetStream stream and
// consumer definitions for work, results, and the dead-letter queue.
//
// The package is named messaging, not nats, so it can import the upstream nats
// client package without an alias collision.
package messaging
