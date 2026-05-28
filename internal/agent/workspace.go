package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Loxstomper/harness/internal/model"
	"github.com/Loxstomper/harness/internal/sandbox"
)

// maxToolOutput caps the bytes a workspace tool feeds back to the model. A `run` of a
// noisy build or a `cat` of a huge file would otherwise blow the context window and
// dilute focus; the head is kept (the leading lines are usually where a command states
// what it is doing) with an explicit truncation marker so the model knows output was cut.
const maxToolOutput = 32 << 10

// WorkspaceTools are the in-sandbox tools that act on the worktree (see
// specs/components/agent.md): read_file, write_file, edit_file, list_dir, search, and
// run (build/test/lint). They all execute through the sandbox's Exec on the candidate
// worktree (working dir = the worktree root); the agent never reaches the host. They
// assume the sandbox profile provides a POSIX shell and coreutils (sh, cat, ls, grep,
// mkdir) — true of the bootstrap Docker images and the planned role rootfses.
//
// A tool that merely failed (file missing, tests red, bad arguments) reports that via
// Outcome.IsError so the model can react and retry; only a sandbox that cannot run a
// command at all is a fatal error that ends the invocation (the runner redelivers).
func WorkspaceTools(sb sandbox.Sandbox) []Tool {
	return []Tool{
		readFileTool(sb),
		writeFileTool(sb),
		editFileTool(sb),
		listDirTool(sb),
		searchTool(sb),
		runTool(sb),
	}
}

func readFileTool(sb sandbox.Sandbox) Tool {
	return funcTool{
		def: model.ToolDef{
			Name:        "read_file",
			Description: "Read a file from the worktree and return its contents.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path relative to the worktree root."}
				},
				"required": ["path"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Path string `json:"path"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Path == "" {
				return invalid("path is required"), nil
			}
			return execTool(ctx, sb, sandbox.Command{Path: "cat", Args: []string{"--", a.Path}})
		},
	}
}

func writeFileTool(sb sandbox.Sandbox) Tool {
	return funcTool{
		def: model.ToolDef{
			Name:        "write_file",
			Description: "Create or overwrite a file in the worktree with the given contents (parent directories are created).",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path relative to the worktree root."},
					"content": {"type": "string", "description": "Full file contents to write."}
				},
				"required": ["path", "content"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Path == "" {
				return invalid("path is required"), nil
			}
			if out, fatal := writeFile(ctx, sb, a.Path, a.Content); fatal != nil {
				return Outcome{}, fatal
			} else if out != nil {
				return *out, nil
			}
			return Outcome{Content: fmt.Sprintf("wrote %s (%d bytes)", a.Path, len(a.Content))}, nil
		},
	}
}

func editFileTool(sb sandbox.Sandbox) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "edit_file",
			Description: "Replace an exact substring in a file. old_string must match exactly once " +
				"unless replace_all is true. Returns an error if old_string is missing or not unique.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path relative to the worktree root."},
					"old_string": {"type": "string", "description": "Exact text to replace."},
					"new_string": {"type": "string", "description": "Replacement text."},
					"replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match."}
				},
				"required": ["path", "old_string", "new_string"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				NewString  string `json:"new_string"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Path == "" {
				return invalid("path is required"), nil
			}
			if a.OldString == "" {
				return invalid("old_string is required"), nil
			}
			if a.OldString == a.NewString {
				return invalid("old_string and new_string are identical"), nil
			}

			content, bad, fatal := readFile(ctx, sb, a.Path)
			if fatal != nil {
				return Outcome{}, fatal
			}
			if bad != nil {
				return *bad, nil
			}

			n := strings.Count(content, a.OldString)
			switch {
			case n == 0:
				return invalid(fmt.Sprintf("old_string not found in %s", a.Path)), nil
			case n > 1 && !a.ReplaceAll:
				return invalid(fmt.Sprintf("old_string is not unique in %s (%d matches); add surrounding context or set replace_all", a.Path, n)), nil
			}

			updated := strings.Replace(content, a.OldString, a.NewString, 1)
			replacements := 1
			if a.ReplaceAll {
				updated = strings.ReplaceAll(content, a.OldString, a.NewString)
				replacements = n
			}
			if out, wfatal := writeFile(ctx, sb, a.Path, updated); wfatal != nil {
				return Outcome{}, wfatal
			} else if out != nil {
				return *out, nil
			}
			return Outcome{Content: fmt.Sprintf("edited %s (%d replacement(s))", a.Path, replacements)}, nil
		},
	}
}

func listDirTool(sb sandbox.Sandbox) Tool {
	return funcTool{
		def: model.ToolDef{
			Name:        "list_dir",
			Description: "List the contents of a directory in the worktree.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "Directory path relative to the worktree root. Defaults to the root."}
				}
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Path string `json:"path"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			path := a.Path
			if path == "" {
				path = "."
			}
			return execTool(ctx, sb, sandbox.Command{Path: "ls", Args: []string{"-la", "--", path}})
		},
	}
}

func searchTool(sb sandbox.Sandbox) Tool {
	return funcTool{
		def: model.ToolDef{
			Name:        "search",
			Description: "Search the worktree for a regular expression, returning matching lines with file and line number.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pattern": {"type": "string", "description": "Regular expression to search for."},
					"path": {"type": "string", "description": "Path to search under, relative to the worktree root. Defaults to the root."}
				},
				"required": ["pattern"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Pattern == "" {
				return invalid("pattern is required"), nil
			}
			path := a.Path
			if path == "" {
				path = "."
			}
			res, err := sb.Exec(ctx, sandbox.Command{Path: "grep", Args: []string{"-rnE", "--", a.Pattern, path}})
			if err != nil {
				return Outcome{}, fmt.Errorf("agent: search exec: %w", err)
			}
			// grep exit 1 means "no lines selected" — a normal, non-error outcome.
			if res.ExitCode == 1 && len(res.Stderr) == 0 {
				return Outcome{Content: "no matches"}, nil
			}
			return formatExec(res), nil
		},
	}
}

func runTool(sb sandbox.Sandbox) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "run",
			Description: "Run a shell command in the worktree (e.g. build, test, or lint). Returns stdout, " +
				"stderr, and the exit code. A non-zero exit is reported but is not a tool failure — read the output.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"command": {"type": "string", "description": "Shell command line to execute in the worktree root."}
				},
				"required": ["command"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Command string `json:"command"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Command == "" {
				return invalid("command is required"), nil
			}
			return execTool(ctx, sb, sandbox.Command{Path: "sh", Args: []string{"-c", a.Command}})
		},
	}
}

// --- shared helpers ----------------------------------------------------------

// execTool runs a command and turns its result into an Outcome. A sandbox that cannot
// run the command at all is fatal (the invocation is redelivered to a fresh sandbox); a
// non-zero exit is data the model reads.
func execTool(ctx context.Context, sb sandbox.Sandbox, cmd sandbox.Command) (Outcome, error) {
	res, err := sb.Exec(ctx, cmd)
	if err != nil {
		return Outcome{}, fmt.Errorf("agent: exec %s: %w", cmd.Path, err)
	}
	return formatExec(res), nil
}

// formatExec renders an ExecResult into the text the model sees: stdout, then stderr
// (labeled), capped at maxToolOutput. A non-zero exit is prefixed and marks the Outcome
// as an error so the model notices it.
func formatExec(res sandbox.ExecResult) Outcome {
	var b strings.Builder
	if len(res.Stdout) > 0 {
		b.Write(res.Stdout)
	}
	if len(res.Stderr) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr]\n")
		b.Write(res.Stderr)
	}
	out := truncate(b.String())
	if res.ExitCode != 0 {
		return Outcome{Content: fmt.Sprintf("exit code %d\n%s", res.ExitCode, out), IsError: true}
	}
	return Outcome{Content: out}
}

// readFile reads a worktree file's contents via the sandbox. It returns the content on
// success; a non-nil *Outcome (IsError) if the file could not be read (e.g. missing) for
// the model to see; or a fatal error if the sandbox could not run cat at all.
func readFile(ctx context.Context, sb sandbox.Sandbox, path string) (string, *Outcome, error) {
	res, err := sb.Exec(ctx, sandbox.Command{Path: "cat", Args: []string{"--", path}})
	if err != nil {
		return "", nil, fmt.Errorf("agent: read %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		o := formatExec(res)
		return "", &o, nil
	}
	return string(res.Stdout), nil, nil
}

// writeFile creates parent dirs and writes content to a worktree file. The path is
// passed as a positional argument (never interpolated into the script) so a path can
// never inject shell; the content arrives on stdin so it is never quoted into the
// command line. Returns a non-nil *Outcome (IsError) if the write command failed, or a
// fatal error if the sandbox could not run it.
func writeFile(ctx context.Context, sb sandbox.Sandbox, path, content string) (*Outcome, error) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Path:  "sh",
		Args:  []string{"-c", `mkdir -p "$(dirname "$1")" && cat > "$1"`, "sh", path},
		Stdin: []byte(content),
	})
	if err != nil {
		return nil, fmt.Errorf("agent: write %s: %w", path, err)
	}
	if res.ExitCode != 0 {
		o := formatExec(res)
		return &o, nil
	}
	return nil, nil
}

// decodeArgs unmarshals a tool's JSON arguments. Malformed arguments are the model's
// mistake to correct, not a fatal invocation error, so they come back as an IsError
// Outcome the model sees on the next turn.
func decodeArgs(args json.RawMessage, v any) *Outcome {
	if len(args) == 0 {
		return nil // no args is valid; required-field checks handle empties
	}
	if err := json.Unmarshal(args, v); err != nil {
		o := invalid(fmt.Sprintf("could not parse arguments: %v", err))
		return &o
	}
	return nil
}

func invalid(msg string) Outcome { return Outcome{Content: msg, IsError: true} }

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	return s[:maxToolOutput] + "\n[output truncated]"
}
