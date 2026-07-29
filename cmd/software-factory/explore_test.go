package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Loxstomper/software-factory/internal/agent"
	"github.com/Loxstomper/software-factory/internal/broker"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/model"
)

// TestExploreToolFor proves the per-role explore gate (T12.5): the `explore` tool is offered to
// an invocation only when BOTH the soul opts in via its `tools` allowlist AND the trusted dispatch
// pinned an explorer for the issue (Brief.Explorer != nil). Missing either gate offers nothing, so
// a soul that never opted in — or an issue explore is disabled for — runs without the tool.
func TestExploreToolFor(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	// The explorer persona is read off the host (absolute by dispatch time), exactly as the loop's
	// bootSoul reads the parent's; a real file is what lets the enabled case actually build.
	personaPath := filepath.Join(t.TempDir(), "explorer.md")
	if err := os.WriteFile(personaPath, []byte("You are a read-only explorer."), 0o600); err != nil {
		t.Fatalf("write explorer persona: %v", err)
	}
	explorer := &core.Soul{Name: "explorer", Role: "explorer", Persona: personaPath}
	// The completer must be a real *broker.Client for the tool to build (the explorer-tagged,
	// per-stream completer source). NewClient does not dial until a call is made, so construction
	// is side-effect-free here.
	client := broker.NewClient("unix", filepath.Join(t.TempDir(), "broker.sock"))

	// A nil sandbox is valid for constructing the read tools (only their Def() is touched at build
	// time; Invoke would need a live sandbox). Sessions is shared with the workspace tools at runtime.
	sessions := agent.NewSessions(nil, log)

	newInv := func(soulTools []string, explorer *core.Soul) agent.Invocation {
		return agent.Invocation{
			Broker:    client,
			Completer: client,
			Brief: core.Brief{
				Issue:         core.Issue{ID: "factory-1", Role: "implementor"},
				Soul:          core.Soul{Name: "implementor", Role: "implementor", Tools: soulTools},
				Explorer:      explorer,
				ExploreBudget: core.ExploreBudget{Tokens: 1000, Turns: 5},
			},
		}
	}

	// Both gates pass: explorer pinned + soul opted in -> offered, and it is the `explore` tool.
	tool, ok := exploreToolFor(newInv([]string{"explore"}, explorer), sessions, log)
	if !ok || tool == nil {
		t.Fatalf("explore should be offered when the soul opts in and an explorer is pinned; got ok=%v tool=%v", ok, tool)
	}
	if name := tool.Def().Name; name != "explore" {
		t.Fatalf("offered tool is %q, want %q", name, "explore")
	}

	// Soul did not opt in (empty allowlist) -> not offered even with a pinned explorer.
	if _, ok := exploreToolFor(newInv(nil, explorer), sessions, log); ok {
		t.Fatal("explore must not be offered when the soul's tools allowlist omits it")
	}

	// No explorer pinned (explore off for this issue) -> not offered even though the soul opted in.
	if _, ok := exploreToolFor(newInv([]string{"explore"}, nil), sessions, log); ok {
		t.Fatal("explore must not be offered when the dispatch pinned no explorer (Brief.Explorer nil)")
	}
}

// TestExploreToolForBuildFailureDegrades proves explore is additive, never load-bearing: when both
// gates pass but the tool cannot be built (here, a non-broker completer that the explorer-completer
// source can't be minted from), the invocation degrades to no explore rather than erroring.
func TestExploreToolForBuildFailureDegrades(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	explorer := &core.Soul{Name: "explorer", Role: "explorer", Persona: "/does/not/matter"}
	inv := agent.Invocation{
		// A fake completer (not *broker.Client) fails the guarded type assertion in buildExploreTool.
		Completer: fakeCompleter{},
		Brief: core.Brief{
			Issue:    core.Issue{ID: "factory-2", Role: "implementor"},
			Soul:     core.Soul{Name: "implementor", Role: "implementor", Tools: []string{"explore"}},
			Explorer: explorer,
		},
	}
	if tool, ok := exploreToolFor(inv, agent.NewSessions(nil, log), log); ok || tool != nil {
		t.Fatalf("a build failure must degrade to no explore; got ok=%v tool=%v", ok, tool)
	}
}

// fakeCompleter is a non-broker agent.Completer so the explore build's guarded *broker.Client
// assertion fails, exercising the additive-degrade path.
type fakeCompleter struct{}

func (fakeCompleter) Complete(_ context.Context, _ model.Request) (model.Response, error) {
	return model.Response{}, nil
}
