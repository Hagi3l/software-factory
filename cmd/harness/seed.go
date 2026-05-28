package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Loxstomper/harness/internal/beads"
	"github.com/Loxstomper/harness/internal/core"
)

// Errors entryRole returns when the entry stage cannot be inferred. They steer the
// operator to pass --role explicitly rather than letting seed guess.
var (
	errNoHarness    = errors.New("config has no harness (DAG) loaded")
	errNoEntryStage = errors.New("no entry agent stage found (every agent stage is produced by another); pass --role")
)

type ambiguousEntryError struct{ roles []string }

func (e *ambiguousEntryError) Error() string {
	return fmt.Sprintf("multiple entry agent stages (%s); pass --role to choose one", strings.Join(e.roles, ", "))
}

// cmdSeed is the CLI stand-in for the requirements wizard: it authors a spec and
// creates one seed issue through the single-writer beads path. Humans own intent —
// this is the only place a human injects work — so it does two things and no more:
// (1) ensure a spec markdown exists (writing a starter from --title/--description if
// the file is absent), and (2) create a beads issue at the entry role, going through
// beads.Apply so the issue is written exactly as the orchestrator would write a
// child (single-writer invariant; the orchestrator is the only other writer). The
// agent reads the spec from its seeded worktree, so the issue body records where the
// spec lives.
func cmdSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	dir := fs.String("config", "config", "config directory (used to resolve the entry role)")
	env := fs.String("env", "dev", "infra environment overlay to load")
	repo := fs.String("repo", ".", "repository holding the beads store (.beads) and specs")
	title := fs.String("title", "", "issue title (required)")
	role := fs.String("role", "", "agent role to enter at (default: the DAG's single entry stage)")
	desc := fs.String("description", "", "issue description / spec summary")
	specPath := fs.String("spec", "", "spec markdown path (relative to --repo); created from --title/--description if absent")
	bdBin := fs.String("bd", "bd", "path to the beads CLI")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*title) == "" {
		return errors.New("--title is required")
	}

	cfg, err := loadConfig(*dir, *env)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	r := *role
	if r == "" {
		if r, err = entryRole(cfg); err != nil {
			return err
		}
	} else if !roleIsAgentStage(cfg, r) {
		return fmt.Errorf("role %q is not an agent stage in the DAG", r)
	}

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}

	body := strings.TrimSpace(*desc)
	if *specPath != "" {
		full := *specPath
		if !filepath.IsAbs(full) {
			full = filepath.Join(absRepo, full)
		}
		if err := ensureSpec(full, *title, *desc); err != nil {
			return err
		}
		// Record the spec location relative to the repo root so the agent (which sees
		// the worktree, not host absolute paths) can find it.
		rel, rerr := filepath.Rel(absRepo, full)
		if rerr != nil {
			rel = *specPath
		}
		if body != "" {
			body += "\n\n"
		}
		body += "Spec: " + filepath.ToSlash(rel)
	}

	bd := beads.New(beads.WithBinary(*bdBin), beads.WithDir(absRepo))
	created, err := bd.Apply(context.Background(), []core.Proposal{{
		Issue: core.Issue{Title: *title, Body: body, Role: r},
	}})
	if err != nil {
		return err
	}
	for _, is := range created {
		fmt.Fprintf(os.Stdout, "seeded issue %s (role %s): %s\n", is.ID, is.Role, is.Title)
	}
	return nil
}

// ensureSpec guarantees a spec markdown exists at path. If it is already there it is
// left untouched (the operator may have authored a richer spec by hand); otherwise a
// starter is written from the title and description so a seed always points at a real
// file. The why over the what: the spec is the durable human intent the agent and
// every later re-derivation read from, so it must exist on disk, not only in the
// issue body.
func ensureSpec(path, title, desc string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", title)
	if strings.TrimSpace(desc) != "" {
		fmt.Fprintf(&b, "%s\n", strings.TrimSpace(desc))
	} else {
		b.WriteString("_Author the requirement here._\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
