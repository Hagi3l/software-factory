// bootstrap-repo prepares a target git project for a factory run.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Loxstomper/software-factory/internal/profile"
)

func cmdBootstrap(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(os.Stdout, `software-factory bootstrap-repo — prepare a target repo for a factory run

usage:
  software-factory bootstrap-repo --repo DIR [--profile go|node|python]

Steps:
  1. Detect (or use --profile) the stack profile
  2. bd init --non-interactive when .beads is missing (requires bd on PATH)
  3. Print docker build + run commands

Does not seed issues or start the pipeline.

`)
		return nil
	}

	repo, prof := "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--repo":
			if i+1 >= len(args) {
				return fmt.Errorf("bootstrap-repo: --repo needs a path")
			}
			i++
			repo = args[i]
		case "--profile":
			if i+1 >= len(args) {
				return fmt.Errorf("bootstrap-repo: --profile needs a name")
			}
			i++
			prof = args[i]
		default:
			return fmt.Errorf("bootstrap-repo: unknown arg %q", args[i])
		}
	}
	if repo == "" {
		return fmt.Errorf("bootstrap-repo: --repo DIR is required")
	}
	abs, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return fmt.Errorf("bootstrap-repo: repo %s is not a directory", abs)
	}
	if _, err := os.Stat(filepath.Join(abs, ".git")); err != nil {
		return fmt.Errorf("bootstrap-repo: %s is not a git repository", abs)
	}

	if prof == "" {
		det, err := profile.Detect(abs)
		if err != nil {
			return err
		}
		if det.Best == "" {
			return fmt.Errorf("bootstrap-repo: could not detect stack; pass --profile go|node|python")
		}
		prof = det.Best
		fmt.Fprintf(os.Stdout, "detected profile: %s (score %d)\n", prof, det.Score)
		if len(det.Evidence) > 0 {
			fmt.Fprintf(os.Stdout, "evidence: %s\n", strings.Join(det.Evidence, "; "))
		}
	}
	info, ok := profile.Known[prof]
	if !ok {
		return fmt.Errorf("bootstrap-repo: unknown profile %q", prof)
	}

	// beads
	beads := filepath.Join(abs, ".beads")
	if _, err := os.Stat(beads); os.IsNotExist(err) {
		if _, lookErr := exec.LookPath("bd"); lookErr != nil {
			fmt.Fprintln(os.Stdout, "warn: bd not on PATH — skip beads init (brew install beads)")
		} else {
			fmt.Fprintln(os.Stdout, "initializing beads store (.beads)…")
			cmd := exec.Command("bd", "init", "--non-interactive")
			cmd.Dir = abs
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("bootstrap-repo: bd init: %w", err)
			}
		}
	} else {
		fmt.Fprintln(os.Stdout, "beads: .beads already present")
	}

	// Node contract hints
	if prof == "node" {
		pj := filepath.Join(abs, "package.json")
		if b, err := os.ReadFile(pj); err == nil {
			s := string(b)
			if !strings.Contains(s, `"test"`) && !strings.Contains(s, `"test:unit"`) {
				fmt.Fprintln(os.Stdout, "warn: package.json has no test/test:unit script — add one (vitest/jest) before author-tests will grade")
			}
		}
	}

	fmt.Fprintln(os.Stdout)
	fmt.Fprintln(os.Stdout, "next steps:")
	fmt.Fprintf(os.Stdout, "  1. software-factory login                    # Grok sub (or set API keys)\n")
	fmt.Fprintf(os.Stdout, "  2. docker build -f %s -t %s .\n", info.Dockerfile, info.Image)
	if prof == "node" && strings.Contains(abs, "get-chilld") {
		fmt.Fprintln(os.Stdout, "     # get-chilld bake (warm node_modules for zero-network):")
		fmt.Fprintln(os.Stdout, "     docker build -f deploy/get-chilld.Dockerfile -t factory/get-chilld:dev "+abs)
		fmt.Fprintf(os.Stdout, "  3. software-factory run --config %s --env get-chilld --repo %s \\\n", info.ConfigDir, abs)
	} else {
		fmt.Fprintf(os.Stdout, "  3. software-factory run --config %s --repo %s \\\n", info.ConfigDir, abs)
	}
	fmt.Fprintln(os.Stdout, "       --serve-addr 127.0.0.1:8080 --nats-addr 127.0.0.1:4222")
	fmt.Fprintln(os.Stdout, "  4. software-factory seed --repo <same> --title \"…\" --description \"…\"")
	fmt.Fprintln(os.Stdout, "  5. software-factory approve --nats nats://127.0.0.1:4222 <issue>   # trusted-dev")
	return nil
}
