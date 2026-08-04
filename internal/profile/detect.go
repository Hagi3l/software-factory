// Package profile discovers shipped language profiles and recommends one for a
// target repository. Profiles are ordinary factory config trees under profiles/<name>/
// (factory.yaml + souls/ + infra.<env>.yaml) — the same shape as config/ and demo/config.
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Known is the built-in catalogue of stack profiles shipped with the factory.
// Keys are directory names under profiles/.
var Known = map[string]Info{
	"go": {
		Name:        "go",
		ConfigDir:   "config", // historical default; self-host Go factory
		Description: "Go modules (go.mod) — self-host profile at config/",
		Sandbox:     "go-toolchain",
		Image:       "factory/go-toolchain:dev",
		Dockerfile:  "deploy/go-toolchain.Dockerfile",
	},
	"node": {
		Name:        "node",
		ConfigDir:   "profiles/node",
		Description: "Node / TypeScript (package.json) — Next.js, pnpm monorepos, etc.",
		Sandbox:     "node-toolchain",
		Image:       "factory/node-toolchain:dev",
		Dockerfile:  "deploy/node-toolchain.Dockerfile",
	},
	"python": {
		Name:        "python",
		ConfigDir:   "profiles/python",
		Description: "Python (pyproject.toml / requirements.txt) — services, libs, automation",
		Sandbox:     "python-toolchain",
		Image:       "factory/python-toolchain:dev",
		Dockerfile:  "deploy/python-toolchain.Dockerfile",
	},
}

// Info describes one stack profile.
type Info struct {
	Name        string
	ConfigDir   string // relative to factory checkout (or absolute if resolved)
	Description string
	Sandbox     string
	Image       string
	Dockerfile  string
}

// List returns known profiles sorted by name.
func List() []Info {
	names := make([]string, 0, len(Known))
	for n := range Known {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]Info, 0, len(names))
	for _, n := range names {
		out = append(out, Known[n])
	}
	return out
}

// DetectResult is a ranked recommendation for a repository.
type DetectResult struct {
	Repo     string
	Best     string   // profile name, or "" if unknown
	Score    int      // evidence weight for Best
	Evidence []string // human-readable signals
	All      map[string]int
}

// Detect inspects repo for stack markers and recommends a profile.
func Detect(repo string) (DetectResult, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return DetectResult{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return DetectResult{}, err
	}
	if !st.IsDir() {
		return DetectResult{}, fmt.Errorf("profile: %s is not a directory", abs)
	}

	scores := map[string]int{}
	var evidence []string

	add := func(profile string, weight int, why string) {
		scores[profile] += weight
		evidence = append(evidence, why)
	}

	// Root markers (strong).
	if exists(abs, "go.mod") {
		add("go", 10, "go.mod")
	}
	if exists(abs, "package.json") {
		add("node", 10, "package.json")
	}
	if exists(abs, "pnpm-lock.yaml") || exists(abs, "pnpm-workspace.yaml") {
		add("node", 3, "pnpm workspace/lock")
	}
	if exists(abs, "yarn.lock") || exists(abs, "package-lock.json") {
		add("node", 2, "npm/yarn lockfile")
	}
	if exists(abs, "pyproject.toml") {
		add("python", 10, "pyproject.toml")
	}
	if exists(abs, "requirements.txt") || exists(abs, "setup.py") {
		add("python", 6, "requirements.txt/setup.py")
	}
	if exists(abs, "Pipfile") || exists(abs, "poetry.lock") {
		add("python", 4, "Pipfile/poetry.lock")
	}

	// Nested monorepo signals (weaker).
	if walkHas(abs, "package.json", 3) && scores["node"] == 0 {
		add("node", 5, "nested package.json")
	}
	if walkHas(abs, "pyproject.toml", 3) && scores["python"] == 0 {
		add("python", 5, "nested pyproject.toml")
	}
	if walkHas(abs, "go.mod", 3) && scores["go"] == 0 {
		add("go", 5, "nested go.mod")
	}

	// Framework hints.
	if exists(abs, "next.config.ts") || exists(abs, "next.config.js") || exists(abs, "next.config.mjs") {
		add("node", 4, "Next.js config")
	}
	if exists(abs, "tsconfig.json") {
		add("node", 2, "tsconfig.json")
	}

	best := ""
	bestScore := 0
	for name, sc := range scores {
		if sc > bestScore {
			best, bestScore = name, sc
		}
	}

	return DetectResult{
		Repo:     abs,
		Best:     best,
		Score:    bestScore,
		Evidence: evidence,
		All:      scores,
	}, nil
}

func exists(root, name string) bool {
	_, err := os.Stat(filepath.Join(root, name))
	return err == nil
}

// walkHas returns true if name appears within maxDepth of root (excluding node_modules/.git/venv).
func walkHas(root, name string, maxDepth int) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		depth := 0
		if rel != "." {
			depth = strings.Count(rel, string(os.PathSeparator)) + 1
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "node_modules" || base == ".venv" || base == "venv" || base == "dist" || base == ".next" {
				return filepath.SkipDir
			}
			if depth > maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == name && depth <= maxDepth {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// ResolveConfigDir returns the config path to pass to --config for a profile name,
// relative to factoryRoot when factoryRoot is non-empty.
func ResolveConfigDir(factoryRoot, profileName string) (string, error) {
	info, ok := Known[profileName]
	if !ok {
		return "", fmt.Errorf("profile: unknown profile %q (want go|node|python)", profileName)
	}
	if factoryRoot == "" {
		return info.ConfigDir, nil
	}
	return filepath.Join(factoryRoot, info.ConfigDir), nil
}
