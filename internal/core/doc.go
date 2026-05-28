// Package core holds the pure domain types shared across the harness — Soul,
// Issue, Brief, and Result — with no behavior and no dependencies on other
// internal packages.
//
// These types are the system's shared vocabulary (see specs/glossary.md). They
// live in a dependency-free leaf package so every component (agent, orchestrator,
// beads, runner, gate) can speak them without risking an import cycle, and so a
// behavioral package like agent never becomes a dependency of a simple one like
// beads merely to share a struct.
package core
