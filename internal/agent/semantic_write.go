package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Loxstomper/software-factory/internal/core"
	"github.com/Loxstomper/software-factory/internal/lsp"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// SemanticWriteTools are the LSP-backed transformation tools (Phase 6, T6.3): rename
// (project-wide) and code_action (the server's own fixes — organize imports, quickfix,
// extract). Like the read tools they are intent-first — the agent states *what* it wants
// and this layer resolves it LSP-first — but unlike the reads they **degrade loudly**: a
// write that silently became a `sed` would corrupt string literals, comments, and the same
// token in the wrong scope undetected (specs/components/agent.md "writes degrade loudly").
//
//   - rename, with a language server, applies the server's precise WorkspaceEdit and records
//     a *semantic* transformation. Without one it falls back to a word-boundary TEXT rename,
//     performed but flagged with an explicit precision warning (match count, files, and a
//     heuristic count of hits inside comments/strings) and recorded as a *text* mechanism —
//     never a silent substring sed.
//   - code_action has no text floor (there is no grep equivalent for "organize imports"), so
//     with no server it refuses loudly rather than guessing.
//
// Every transformation's mechanism is recorded in the shared ledger so the terminal submit
// folds it into the Result's evidence — provenance the gate and a reviewer can weigh (a
// text-fallback rename more than a semantic one). The ledger may be nil (no recording).
//
// Positions on the tool surface are 1-based line+column (matching find_symbol/search); this
// layer translates to the session's 0-based LSP positions and back, the same as the reads.
func SemanticWriteTools(s *Sessions, ledger *TransformLedger) []Tool {
	return []Tool{
		renameTool(s, ledger),
		codeActionTool(s, ledger),
	}
}

func renameTool(s *Sessions, ledger *TransformLedger) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "rename",
			Description: "Rename the symbol at a position everywhere it is used, project-wide, using the " +
				"language server for a precise rename. Use a position from find_symbol or search. If no " +
				"language server is available it falls back to a word-boundary text rename and warns that the " +
				"result is unverified (it cannot tell code from comments or strings) — review it.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path relative to the worktree root."},
					"line": {"type": "integer", "description": "1-based line of the symbol to rename (from find_symbol or search)."},
					"character": {"type": "integer", "description": "1-based column of the symbol (from find_symbol)."},
					"new_name": {"type": "string", "description": "The new identifier name."}
				},
				"required": ["path", "line", "character", "new_name"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Path      string `json:"path"`
				Line      int    `json:"line"`
				Character int    `json:"character"`
				NewName   string `json:"new_name"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			switch {
			case a.Path == "":
				return invalid("path is required"), nil
			case a.Line < 1:
				return invalid("line is required (1-based, as shown by find_symbol or search)"), nil
			case a.Character < 1:
				return invalid("character is required (1-based column, as shown by find_symbol)"), nil
			case a.NewName == "":
				return invalid("new_name is required"), nil
			case !isIdentifier(a.NewName):
				return invalid(fmt.Sprintf("new_name %q is not a valid identifier", a.NewName)), nil
			}

			we, err := s.Rename(ctx, a.Path, a.Line-1, a.Character-1, a.NewName)
			if err != nil {
				// No language server (or the server refused): degrade LOUDLY to a text rename.
				return textRename(ctx, s, ledger, a.Path, a.Line, a.Character, a.NewName)
			}
			files, edits, aerr := s.applyWorkspaceEdit(ctx, we)
			if aerr != nil {
				// The sandbox could not read/write the worktree — a broken sandbox is fatal,
				// the same posture as the text-floor edit tools (the runner redelivers).
				return Outcome{}, aerr
			}
			if files == 0 {
				return Outcome{Content: fmt.Sprintf("the language server proposed no changes to rename the symbol at %s:%d:%d", a.Path, a.Line, a.Character)}, nil
			}
			ledger.Record(core.TransformRecord{
				Tool:      "rename",
				Target:    fmt.Sprintf("%s:%d:%d → %s", a.Path, a.Line, a.Character, a.NewName),
				Mechanism: core.TransformMechanismSemantic,
				Files:     files,
				Edits:     edits,
			})
			return Outcome{Content: fmt.Sprintf("renamed to %q via the language server: %d edit(s) across %d file(s)", a.NewName, edits, files)}, nil
		},
	}
}

// textRename is the LOUD degrade for rename when no language server can serve the file: a
// word-boundary text replacement of the identifier at the position, performed but reported
// with an explicit precision warning so the model never mistakes it for a precise rename.
// Word boundaries mean substrings are not touched (the one corruption class text *can*
// avoid structurally); occurrences inside comments and string literals ARE rewritten (text
// cannot tell them apart), so they are counted heuristically and surfaced. The mechanism is
// recorded as text so the provenance trail weighs it accordingly.
func textRename(ctx context.Context, s *Sessions, ledger *TransformLedger, relPath string, line, char int, newName string) (Outcome, error) {
	content, err := s.readFile(ctx, relPath)
	if err != nil {
		return invalid(fmt.Sprintf("no language server available and could not read %s for a text rename: %v", relPath, err)), nil
	}
	old := identifierAt(content, line, char)
	if old == "" {
		return invalid(fmt.Sprintf("no language server available and no identifier found at %s:%d:%d to rename", relPath, line, char)), nil
	}
	if old == newName {
		return invalid(fmt.Sprintf("the symbol at %s:%d:%d is already named %q", relPath, line, char, newName)), nil
	}

	re := regexp.MustCompile(`\b` + regexp.QuoteMeta(old) + `\b`)
	files, ferr := s.matchingFiles(ctx, re)
	if ferr != nil {
		return Outcome{}, ferr // grep could not run at all — broken sandbox, fatal
	}

	var changedFiles, totalMatches, risky int
	for _, f := range files {
		c, rerr := s.readFile(ctx, f)
		if rerr != nil {
			continue // best-effort: a file we listed but cannot read is skipped
		}
		if strings.IndexByte(c, 0) >= 0 {
			continue // binary file: never rewrite (a chance byte match is not a rename)
		}
		spans := re.FindAllStringIndex(c, -1)
		if len(spans) == 0 {
			continue
		}
		for _, span := range spans {
			lineText, col := lineColAt(c, span[0])
			if riskyMatch(lineText, col) {
				risky++
			}
		}
		updated := re.ReplaceAllString(c, newName)
		if out, werr := writeFile(ctx, s.exec, f, updated); werr != nil {
			return Outcome{}, werr
		} else if out != nil {
			return Outcome{}, fmt.Errorf("agent: text rename: write %s: %s", f, out.Content)
		}
		s.NotifyEdit(ctx, f, updated)
		totalMatches += len(spans)
		changedFiles++
	}

	if changedFiles == 0 {
		return Outcome{Content: fmt.Sprintf("[unverified: text rename] no occurrences of %q found to rename", old)}, nil
	}

	note := fmt.Sprintf("%d match(es) across %d file(s)", totalMatches, changedFiles)
	if risky > 0 {
		note += fmt.Sprintf("; %d inside comments or string literals (heuristic) — review them", risky)
	}
	ledger.Record(core.TransformRecord{
		Tool:      "rename",
		Target:    fmt.Sprintf("%s → %s at %s:%d:%d", old, newName, relPath, line, char),
		Mechanism: core.TransformMechanismText,
		Files:     changedFiles,
		Edits:     totalMatches,
		Note:      note,
	})
	return Outcome{Content: fmt.Sprintf(
		"[unverified: no language server — performed a word-boundary TEXT rename of %q → %q. %s. "+
			"A text rename cannot distinguish code from comments or string literals; verify the changes.]",
		old, newName, note)}, nil
}

func codeActionTool(s *Sessions, ledger *TransformLedger) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "code_action",
			Description: "Apply one of the language server's own fixes for a file or range (organize imports, " +
				"add a missing import, a quickfix, an extract). Omit title/kind to list the available actions; " +
				"pass title (exact or substring) or kind (e.g. \"source.organizeImports\", \"quickfix\") to apply one. " +
				"Requires a language server — there is no text fallback for a server-computed fix.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"path": {"type": "string", "description": "File path relative to the worktree root."},
					"line": {"type": "integer", "description": "1-based line to act at (from a diagnostic or find_symbol). Omit to consider the whole file."},
					"character": {"type": "integer", "description": "1-based column. Defaults to 1."},
					"end_line": {"type": "integer", "description": "1-based end line of the range. Defaults to the start line."},
					"end_character": {"type": "integer", "description": "1-based end column. Defaults to the start column."},
					"title": {"type": "string", "description": "Select the offered action whose title matches (exact or substring). Omit to list actions."},
					"kind": {"type": "string", "description": "Select an action by kind prefix, e.g. \"source.organizeImports\" or \"quickfix\"."}
				},
				"required": ["path"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Path         string `json:"path"`
				Line         int    `json:"line"`
				Character    int    `json:"character"`
				EndLine      int    `json:"end_line"`
				EndCharacter int    `json:"end_character"`
				Title        string `json:"title"`
				Kind         string `json:"kind"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Path == "" {
				return invalid("path is required"), nil
			}

			rng, bad := s.codeActionRange(ctx, a.Path, a.Line, a.Character, a.EndLine, a.EndCharacter)
			if bad != nil {
				return *bad, nil
			}

			actions, err := s.CodeAction(ctx, a.Path, rng)
			if err != nil {
				// No text floor for a server-computed fix: refuse LOUDLY (IsError) so the model
				// reacts, rather than silently pretending nothing was offered. The error is a
				// degrade signal (no semantic session), not a fatal invocation fault.
				//nolint:nilerr // intentional loud degrade: a missing language server is not an invocation error
				return Outcome{
					Content: "[no language server available] code actions (organize imports, quickfix, extract) require a language " +
						"server; none is available for this file. Run gofmt/goimports via `run`, or make the change with edit_file.",
					IsError: true,
				}, nil
			}
			if len(actions) == 0 {
				return Outcome{Content: "no code actions are available for that range"}, nil
			}

			selected, listing := selectAction(actions, a.Title, a.Kind)
			if selected == nil {
				if a.Title == "" && a.Kind == "" {
					return Outcome{Content: "available code actions (pass title or kind to apply one):\n" + listing}, nil
				}
				return Outcome{Content: "no single code action matched; available actions:\n" + listing + "\nspecify a title or kind that matches exactly one", IsError: true}, nil
			}
			if selected.Edit == nil {
				// The action is an opaque server command, not an inline edit; executing server
				// commands is out of scope (the protocol keeps Command raw), so report it.
				return Outcome{Content: fmt.Sprintf("code action %q has no inline edit (it is a server command, which this tool does not execute)", selected.Title), IsError: true}, nil
			}
			files, edits, aerr := s.applyWorkspaceEdit(ctx, *selected.Edit)
			if aerr != nil {
				return Outcome{}, aerr
			}
			if files == 0 {
				return Outcome{Content: fmt.Sprintf("code action %q proposed no changes", selected.Title)}, nil
			}
			ledger.Record(core.TransformRecord{
				Tool:      "code_action",
				Target:    selected.Title,
				Mechanism: core.TransformMechanismSemantic,
				Files:     files,
				Edits:     edits,
			})
			return Outcome{Content: fmt.Sprintf("applied %q via the language server: %d edit(s) across %d file(s)", selected.Title, edits, files)}, nil
		},
	}
}

// --- WorkspaceEdit application ----------------------------------------------------

// applyWorkspaceEdit writes a language-server WorkspaceEdit to the worktree and re-syncs
// each changed file into the running session (so a follow-up diagnostics/references reads
// the new text). Files are processed in sorted order for determinism; a read/write fault is
// returned as a fatal error (a broken sandbox, like the text-floor edit tools). It returns
// the number of files and individual edits applied.
func (s *Sessions) applyWorkspaceEdit(ctx context.Context, we lsp.WorkspaceEdit) (files, edits int, err error) {
	byFile := we.Files()
	uris := make([]string, 0, len(byFile))
	for uri := range byFile {
		uris = append(uris, uri)
	}
	sort.Strings(uris)
	for _, uri := range uris {
		tedits := byFile[uri]
		if len(tedits) == 0 {
			continue
		}
		rel := relForURI(s.root, uri)
		content, rerr := s.readFile(ctx, rel)
		if rerr != nil {
			return files, edits, fmt.Errorf("agent: apply edit: read %s: %w", rel, rerr)
		}
		updated := applyTextEdits(content, tedits)
		if updated == content {
			continue
		}
		if out, werr := writeFile(ctx, s.exec, rel, updated); werr != nil {
			return files, edits, fmt.Errorf("agent: apply edit: write %s: %w", rel, werr)
		} else if out != nil {
			return files, edits, fmt.Errorf("agent: apply edit: write %s: %s", rel, out.Content)
		}
		s.NotifyEdit(ctx, rel, updated)
		files++
		edits += len(tedits)
	}
	return files, edits, nil
}

// applyTextEdits applies a document's TextEdits to its content. LSP guarantees edits within
// a document do not overlap; applying them in descending start order means each splice only
// touches the tail beyond lower-offset edits, so earlier offsets stay valid throughout.
func applyTextEdits(content string, edits []lsp.TextEdit) string {
	type span struct {
		start, end int
		text       string
	}
	spans := make([]span, 0, len(edits))
	for _, e := range edits {
		start := positionToOffset(content, e.Range.Start)
		end := positionToOffset(content, e.Range.End)
		if end < start {
			end = start
		}
		spans = append(spans, span{start, end, e.NewText})
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start > spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	res := content
	for _, sp := range spans {
		if sp.start < 0 || sp.end > len(res) {
			continue
		}
		res = res[:sp.start] + sp.text + res[sp.end:]
	}
	return res
}

// positionToOffset converts a 0-based LSP Position (line + UTF-16 code-unit column) into a
// byte offset into content. A line past EOF clamps to len(content); a column past the line
// end clamps to the line's newline. Go source is mostly ASCII (column == byte), but the
// UTF-16 accounting keeps an edit landing in or after a non-ASCII string literal correct.
func positionToOffset(content string, pos lsp.Position) int {
	off := 0
	for l := 0; l < pos.Line; l++ {
		nl := strings.IndexByte(content[off:], '\n')
		if nl < 0 {
			return len(content)
		}
		off += nl + 1
	}
	units := 0
	for off < len(content) {
		if units >= pos.Character {
			return off
		}
		r, sz := utf8.DecodeRuneInString(content[off:])
		if r == '\n' {
			return off
		}
		if r > 0xFFFF {
			units += 2 // surrogate pair in UTF-16
		} else {
			units++
		}
		off += sz
	}
	return off
}

// --- code-action helpers ----------------------------------------------------------

// codeActionRange builds the LSP range for a code action from 1-based tool arguments. With
// no start line it spans the whole file (the natural scope for organize-imports), reading
// the file to find its end; otherwise it spans the given start to the given end (end
// defaulting to the start). A read fault for the whole-file case is a recoverable Outcome.
func (s *Sessions) codeActionRange(ctx context.Context, relPath string, line, char, endLine, endChar int) (lsp.Range, *Outcome) {
	if line < 1 {
		content, err := s.readFile(ctx, relPath)
		if err != nil {
			o := invalid(fmt.Sprintf("could not read %s: %v", relPath, err))
			return lsp.Range{}, &o
		}
		return fullRange(content), nil
	}
	start := lsp.Position{Line: line - 1, Character: max0(char - 1)}
	end := start
	if endLine >= 1 {
		end = lsp.Position{Line: endLine - 1, Character: max0(endChar - 1)}
	}
	return lsp.Range{Start: start, End: end}, nil
}

// fullRange returns the range covering an entire document (start of file to the end of the
// last line), in 0-based LSP positions.
func fullRange(content string) lsp.Range {
	lines := strings.Split(content, "\n")
	last := len(lines) - 1
	return lsp.Range{
		Start: lsp.Position{Line: 0, Character: 0},
		End:   lsp.Position{Line: last, Character: utf16Len(lines[last])},
	}
}

// selectAction picks the single code action matching title (exact or case-insensitive
// substring) or kind (exact or prefix), returning it plus a human-readable listing of all
// offered actions. With no selector it returns the sole action (if there is exactly one) or
// nil (ambiguous). title takes precedence over kind. A nil return means "list, don't apply".
func selectAction(actions []lsp.CodeAction, title, kind string) (*lsp.CodeAction, string) {
	var b strings.Builder
	for i := range actions {
		fmt.Fprintf(&b, "- %s", actions[i].Title)
		if actions[i].Kind != "" {
			fmt.Fprintf(&b, " [%s]", actions[i].Kind)
		}
		b.WriteByte('\n')
	}
	listing := strings.TrimRight(b.String(), "\n")

	if title == "" && kind == "" {
		if len(actions) == 1 {
			return &actions[0], listing
		}
		return nil, listing
	}

	var matched *lsp.CodeAction
	count := 0
	for i := range actions {
		a := &actions[i]
		var hit bool
		if title != "" {
			hit = a.Title == title || strings.Contains(strings.ToLower(a.Title), strings.ToLower(title))
		} else {
			hit = a.Kind == kind || strings.HasPrefix(a.Kind, kind)
		}
		if hit {
			matched = a
			count++
		}
	}
	if count == 1 {
		return matched, listing
	}
	return nil, listing
}

// --- text-rename helpers ----------------------------------------------------------

// matchingFiles lists the worktree files containing a match for re (the word-boundary
// pattern of the identifier being renamed), excluding the .git directory. A grep that
// cannot run at all is a fatal error (broken sandbox); "no matches" is an empty list.
func (s *Sessions) matchingFiles(ctx context.Context, re *regexp.Regexp) ([]string, error) {
	res, err := s.exec.Exec(ctx, sandbox.Command{Path: "grep", Args: []string{"-rlE", "--", re.String(), "."}})
	if err != nil {
		return nil, fmt.Errorf("agent: text rename: list files: %w", err)
	}
	if res.ExitCode == 1 && len(res.Stderr) == 0 {
		return nil, nil // no matches
	}
	if res.ExitCode > 1 {
		return nil, fmt.Errorf("agent: text rename: grep exit %d: %s", res.ExitCode, string(res.Stderr))
	}
	var out []string
	for _, ln := range strings.Split(string(res.Stdout), "\n") {
		f := strings.TrimPrefix(strings.TrimSpace(ln), "./")
		if f == "" || f == ".git" || strings.HasPrefix(f, ".git/") || strings.Contains(f, "/.git/") {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// lineColAt returns the text of the line containing byte offset off, and off's byte column
// within that line (0-based). Used to classify a match for the comment/string heuristic.
func lineColAt(content string, off int) (string, int) {
	start := strings.LastIndexByte(content[:off], '\n') + 1 // 0 when there is no preceding newline
	end := strings.IndexByte(content[off:], '\n')
	if end < 0 {
		end = len(content)
	} else {
		end += off
	}
	return content[start:end], off - start
}

// riskyMatch reports whether byte column col on line falls inside a string literal or after
// an unquoted line comment (`//`). It is a deliberately simple, single-line heuristic for
// the text-rename precision warning — it does not track block comments or multi-line strings
// — so it is reported as a heuristic, never as a guarantee.
func riskyMatch(line string, col int) bool {
	var quote byte // 0 = not in a string, else the opening quote char
	for i := 0; i < col && i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == '\\' {
				i++ // skip the escaped char
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'', '`':
			quote = c
		case '/':
			if i+1 < len(line) && line[i+1] == '/' {
				return true // a line comment starts before col
			}
		}
	}
	return quote != 0
}

// --- small utilities --------------------------------------------------------------

// isIdentifier reports whether s is a valid identifier (letter/underscore, then
// letters/digits/underscores). It guards the rename's new name so a text fallback never
// writes obviously-malformed tokens into the worktree.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r):
		case i > 0 && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return true
}

// utf16Len returns the number of UTF-16 code units in s (the LSP column unit).
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
