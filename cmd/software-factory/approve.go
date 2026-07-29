package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"

	"github.com/nats-io/nats.go"

	"github.com/Loxstomper/software-factory/internal/beads"
	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/messaging"
)

// cmdApprove and cmdReject are the human's approval levers for the trusted-dev / TCB-review
// gate (T2.10). An integrate candidate held for approval is parked (blocked) carrying the
// candidate ref it awaits; these commands read that ref and publish the human's decision over
// NATS for the single-writer orchestrator to apply — the same propose-don't-write discipline
// `software-factory seed` and an agent's Result follow (a human never mutates beads directly during a
// run; only the orchestrator does). Approve resumes the merge; reject routes a fix (or, with
// no route/budget left, dead-letters for spec refinement). The decision is bound to the
// candidate sha, so an approval for a candidate that has since changed is invalidated.
func cmdApprove(args []string) error { return runApprovalDecision("approve", args, true) }
func cmdReject(args []string) error  { return runApprovalDecision("reject", args, false) }

// runApprovalDecision is the shared body: read the parked issue's candidate ref, then publish
// an ApprovalRequest. It connects to a RUNNING factory's NATS (the run must expose a client
// address via `software-factory run --nats-addr`; the default in-process server is unreachable from a
// separate process — see specs/messaging.md, T5.8), so a failed connect is the common "is the
// factory running and listening?" error and is surfaced plainly.
func runApprovalDecision(name string, args []string, approved bool) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	repo := fs.String("repo", ".", "repository holding the beads store (.beads)")
	bdBin := fs.String("bd", "bd", "path to the beads CLI")
	natsURL := fs.String("nats", "nats://127.0.0.1:4222", "URL of the running factory's NATS client listener (software-factory run --nats-addr)")
	approver := fs.String("approver", "", "who is deciding (default: the OS user); recorded on the issue for audit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	issueID := fs.Arg(0)
	if issueID == "" {
		return fmt.Errorf("usage: factory %s [--nats URL] [--approver WHO] <issue>", name)
	}

	absRepo, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}

	// Read the candidate the human is deciding on, so the decision is bound to the exact sha
	// they reviewed. This is a read-only beads access; the orchestrator stays the single writer.
	bd := beads.New(beads.WithBinary(*bdBin), beads.WithDir(absRepo))
	issue, err := bd.Get(context.Background(), issueID)
	if err != nil {
		return fmt.Errorf("read issue %s: %w", issueID, err)
	}
	if issue.CandidateRef == "" {
		return fmt.Errorf("issue %s is not awaiting approval (no parked candidate); only a parked integrate can be approved or rejected", issueID)
	}

	req := core.ApprovalRequest{
		IssueID:      issueID,
		CandidateSHA: issue.CandidateRef,
		Approved:     approved,
		Approver:     resolveApprover(*approver),
	}
	if err := publishApproval(*natsURL, req); err != nil {
		return err
	}

	verb := "approved"
	if !approved {
		verb = "rejected"
	}
	fmt.Fprintf(os.Stdout, "%s issue %s (candidate %s) by %s\n", verb, issueID, issue.CandidateRef, req.Approver)
	return nil
}

// publishApproval connects to the running factory's NATS and publishes the decision onto the
// durable approvals stream, so it survives until the orchestrator consumes it (at-least-once).
func publishApproval(url string, req core.ApprovalRequest) error {
	nc, err := nats.Connect(url)
	if err != nil {
		return fmt.Errorf("connect to factory NATS at %s (is `software-factory run --nats-addr %s` running?): %w", url, addrOf(url), err)
	}
	defer nc.Close()

	js, err := messaging.JetStream(nc)
	if err != nil {
		return err
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := js.Publish(ctx, messaging.SubjectApprovals, data); err != nil {
		return fmt.Errorf("publish approval for %s: %w", req.IssueID, err)
	}
	return nil
}

// resolveApprover defaults an unset --approver to the OS user, so an approval always carries
// an accountable identity in the issue's audit record without forcing the flag.
func resolveApprover(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown"
}

// addrOf strips a nats:// scheme for the hint in the connect-error message; best-effort.
func addrOf(url string) string {
	const scheme = "nats://"
	if len(url) > len(scheme) && url[:len(scheme)] == scheme {
		return url[len(scheme):]
	}
	return url
}
