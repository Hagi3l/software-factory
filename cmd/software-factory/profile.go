// Profile subcommands: list shipped stack profiles and detect which fits a repo.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Loxstomper/software-factory/internal/profile"
)

func cmdProfile(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(os.Stdout, `software-factory profile — stack profiles for multi-language factories

The kernel is language-agnostic; a *profile* is a full config tree (DAG + checks +
souls + sandbox image) for one ecosystem. Shipped profiles:

  go      config/            (self-host; Go gates)
  node    profiles/node      (package.json / Next / pnpm monorepos)
  python  profiles/python    (pyproject.toml / pytest / ruff)

usage:
  software-factory profile list
  software-factory profile detect --repo DIR
  software-factory profile show NAME

After detect, run with the recommended --config and build the profile image:

  docker build -f deploy/node-toolchain.Dockerfile -t factory/node-toolchain:dev .
  software-factory run --config profiles/node --repo /path/to/app ...

See profiles/README.md and docs/profiles.md.

`)
		return nil
	}
	switch args[0] {
	case "list":
		return profileList(args[1:])
	case "detect":
		return profileDetect(args[1:])
	case "show":
		return profileShow(args[1:])
	default:
		return fmt.Errorf("profile: unknown subcommand %q (want list|detect|show)", args[0])
	}
}

func profileList(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprintln(os.Stdout, "software-factory profile list — print shipped stack profiles")
		return nil
	}
	for _, p := range profile.List() {
		fmt.Fprintf(os.Stdout, "%-8s  %-18s  %s\n", p.Name, p.ConfigDir, p.Description)
		fmt.Fprintf(os.Stdout, "          image %s  (build: %s)\n", p.Image, p.Dockerfile)
	}
	return nil
}

func profileDetect(args []string) error {
	repo := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			fmt.Fprint(os.Stdout, "software-factory profile detect --repo DIR — recommend a stack profile\n")
			return nil
		case "--repo":
			if i+1 >= len(args) {
				return fmt.Errorf("profile detect: --repo needs a path")
			}
			i++
			repo = args[i]
		default:
			if repo == "" && !strings.HasPrefix(args[i], "-") {
				repo = args[i]
				continue
			}
			return fmt.Errorf("profile detect: unknown arg %q", args[i])
		}
	}
	if repo == "" {
		return fmt.Errorf("profile detect: --repo DIR is required")
	}
	r, err := profile.Detect(repo)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "repo: %s\n", r.Repo)
	if r.Best == "" {
		fmt.Fprintln(os.Stdout, "recommended: (none — no stack markers found)")
		fmt.Fprintln(os.Stdout, "hint: add package.json, pyproject.toml, or go.mod, or pick a profile manually")
		return nil
	}
	info := profile.Known[r.Best]
	fmt.Fprintf(os.Stdout, "recommended: %s (score %d)\n", r.Best, r.Score)
	fmt.Fprintf(os.Stdout, "config:      %s\n", info.ConfigDir)
	fmt.Fprintf(os.Stdout, "image:       %s\n", info.Image)
	fmt.Fprintf(os.Stdout, "build:       docker build -f %s -t %s .\n", info.Dockerfile, info.Image)
	if len(r.Evidence) > 0 {
		fmt.Fprintf(os.Stdout, "evidence:    %s\n", strings.Join(r.Evidence, "; "))
	}
	if len(r.All) > 1 {
		names := make([]string, 0, len(r.All))
		for n := range r.All {
			names = append(names, n)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s=%d", n, r.All[n]))
		}
		fmt.Fprintf(os.Stdout, "all scores:  %s\n", strings.Join(parts, " "))
	}
	fmt.Fprintln(os.Stdout)
	fmt.Fprintf(os.Stdout, "next:\n  bd init --non-interactive   # in the target repo if needed\n")
	fmt.Fprintf(os.Stdout, "  docker build -f %s -t %s .\n", info.Dockerfile, info.Image)
	fmt.Fprintf(os.Stdout, "  software-factory run --config %s --repo %s --serve-addr 127.0.0.1:8080\n",
		info.ConfigDir, r.Repo)
	return nil
}

func profileShow(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stdout, "software-factory profile show NAME — details for one profile")
		return nil
	}
	name := args[0]
	info, ok := profile.Known[name]
	if !ok {
		return fmt.Errorf("profile: unknown %q", name)
	}
	fmt.Fprintf(os.Stdout, "name:        %s\n", info.Name)
	fmt.Fprintf(os.Stdout, "config:      %s\n", info.ConfigDir)
	fmt.Fprintf(os.Stdout, "description: %s\n", info.Description)
	fmt.Fprintf(os.Stdout, "sandbox:     %s\n", info.Sandbox)
	fmt.Fprintf(os.Stdout, "image:       %s\n", info.Image)
	fmt.Fprintf(os.Stdout, "dockerfile:  %s\n", info.Dockerfile)
	// If we can find the factory root (cwd or executable-relative), print absolute config.
	if wd, err := os.Getwd(); err == nil {
		cand := filepath.Join(wd, info.ConfigDir)
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			fmt.Fprintf(os.Stdout, "resolved:    %s\n", cand)
		}
	}
	return nil
}
