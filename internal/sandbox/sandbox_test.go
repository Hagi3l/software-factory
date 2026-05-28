package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Loxstomper/harness/internal/config"
)

func validSpec() Spec {
	return Spec{
		Profile:   "go-toolchain",
		Workspace: Workspace{Repo: "/srv/repo.git", BaseRef: "main"},
		Limits: config.SandboxLimits{
			CPU:  2,
			Mem:  "2Gi",
			Wall: config.Duration(30 * time.Minute),
		},
		Broker: Endpoint{Network: "unix", Address: "/run/harness/broker.sock"},
	}
}

func TestSpecValidateHappyPath(t *testing.T) {
	if err := validSpec().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// Disk is optional — a spec without it is still valid.
func TestSpecValidateDiskOptional(t *testing.T) {
	s := validSpec()
	s.Limits.Disk = ""
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate rejected an empty disk: %v", err)
	}
}

func TestSpecValidateProblems(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mutate   func(*Spec)
		fragment string
	}{
		{"no profile", func(s *Spec) { s.Profile = "" }, "profile is empty"},
		{"no repo", func(s *Spec) { s.Workspace.Repo = "" }, "workspace repo is empty"},
		{"no base ref", func(s *Spec) { s.Workspace.BaseRef = "" }, "base ref is empty"},
		{"no broker net", func(s *Spec) { s.Broker.Network = "" }, "broker network is empty"},
		{"no broker addr", func(s *Spec) { s.Broker.Address = "" }, "broker address is empty"},
		{"zero cpu", func(s *Spec) { s.Limits.CPU = 0 }, "cpu must be positive"},
		{"no mem", func(s *Spec) { s.Limits.Mem = "" }, "mem is empty"},
		{"zero wall", func(s *Spec) { s.Limits.Wall = 0 }, "wall must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := validSpec()
			tc.mutate(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("Validate passed for %s, want failure", tc.name)
			}
			var ve *InvalidSpecError
			if !errors.As(err, &ve) {
				t.Fatalf("error is %T, want *InvalidSpecError", err)
			}
			found := false
			for _, p := range ve.Problems {
				if strings.Contains(p, tc.fragment) {
					found = true
				}
			}
			if !found {
				t.Errorf("no problem contained %q; got %v", tc.fragment, ve.Problems)
			}
		})
	}
}

// Validate reports every problem at once, sorted for deterministic output.
func TestSpecValidateReportsAllSorted(t *testing.T) {
	err := Spec{}.Validate()
	var ve *InvalidSpecError
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want *InvalidSpecError", err)
	}
	if len(ve.Problems) < 2 {
		t.Fatalf("want multiple problems for an empty spec, got %v", ve.Problems)
	}
	for i := 1; i < len(ve.Problems); i++ {
		if ve.Problems[i-1] > ve.Problems[i] {
			t.Errorf("problems not sorted: %q before %q", ve.Problems[i-1], ve.Problems[i])
		}
	}
}

// --- Interface satisfiability: a fake backend exercising the full lifecycle ---

// fakeSandbox is an in-test implementation proving the Sandbox/Backend interfaces
// compose into the intended provision -> exec -> teardown lifecycle. It is not a
// production backend (that is the Docker backend, T1.6).
type fakeSandbox struct {
	id        string
	spec      Spec
	execCalls []Command
	tornDown  int
}

func (s *fakeSandbox) ID() string { return s.id }

func (s *fakeSandbox) Exec(_ context.Context, cmd Command) (ExecResult, error) {
	s.execCalls = append(s.execCalls, cmd)
	return ExecResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

func (s *fakeSandbox) Teardown(_ context.Context) error {
	s.tornDown++ // count to assert idempotency is at least callable repeatedly
	return nil
}

type fakeBackend struct{ provisioned []*fakeSandbox }

func (b *fakeBackend) Provision(_ context.Context, spec Spec) (Sandbox, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	s := &fakeSandbox{id: "sbx-1", spec: spec}
	b.provisioned = append(b.provisioned, s)
	return s, nil
}

// Compile-time assertions that the fakes satisfy the interfaces.
var (
	_ Backend = (*fakeBackend)(nil)
	_ Sandbox = (*fakeSandbox)(nil)
)

func TestBackendLifecycle(t *testing.T) {
	ctx := context.Background()
	var be Backend = &fakeBackend{}

	sb, err := be.Provision(ctx, validSpec())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if sb.ID() == "" {
		t.Error("sandbox has no ID")
	}

	res, err := sb.Exec(ctx, Command{Path: "go", Args: []string{"test", "./..."}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "ok" {
		t.Errorf("exec result = %+v", res)
	}

	if err := sb.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	// Teardown must be safe to call again (idempotent contract).
	if err := sb.Teardown(ctx); err != nil {
		t.Fatalf("second Teardown: %v", err)
	}
}

// A backend must reject an invalid spec before allocating anything.
func TestBackendRejectsInvalidSpec(t *testing.T) {
	be := &fakeBackend{}
	if _, err := be.Provision(context.Background(), Spec{}); err == nil {
		t.Fatal("Provision accepted an invalid spec")
	}
	if len(be.provisioned) != 0 {
		t.Errorf("backend allocated a sandbox for an invalid spec: %d", len(be.provisioned))
	}
}
