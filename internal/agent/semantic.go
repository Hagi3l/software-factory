package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/Loxstomper/software-factory/internal/lsp"
	"github.com/Loxstomper/software-factory/internal/model"
	"github.com/Loxstomper/software-factory/internal/sandbox"
)

// SemanticReadTools are the LSP-backed comprehension tools (Phase 6, T6.2): find_symbol,
// references, definition, implementation, hover, and diagnostics. They are intent-first —
// the agent states *what* it wants (find this symbol, who references it) and this layer
// resolves it **LSP-first against the warm session, falling back to a text search**. The
// agent never picks the mechanism, so "prefer semantic, fall back to grep" is a structural
// property of the tool, not a persona nudge (specs/components/agent.md "Semantic tools").
//
// Reads degrade *silently* to the text floor: when no language server can serve the file
// (no streamed-session support, no manifest, or no entry for the language), the tool greps
// for the relevant identifier and labels the result **unverified** so the model knows
// precision dropped — worst case is exactly today's `search`. (Writes degrade loudly; that
// is T6.3.) The text-floor tools (read_file/list_dir/search) remain available and now point
// at these for code navigation.
//
// All positions the tools accept and emit are **1-based line and column**, matching what
// find_symbol and search print, so a location from one tool feeds straight into the next.
// The session-internal LSP positions are 0-based; this layer does the translation.
func SemanticReadTools(s *Sessions) []Tool {
	return []Tool{
		findSymbolTool(s),
		referencesTool(s),
		definitionTool(s),
		implementationTool(s),
		hoverTool(s),
		diagnosticsTool(s),
	}
}

func findSymbolTool(s *Sessions) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "find_symbol",
			Description: "Locate a symbol (function, type, method, variable, constant) across the whole " +
				"project by name, using the language server for precise results. Prefer this over `search` " +
				"when you know the symbol's name. Returns each match as `Kind Name — path:line:column`.",
			Params: json.RawMessage(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Symbol name to locate (exact or fuzzy, as the language server supports)."},
					"language": {"type": "string", "description": "Language id of the symbol (default \"go\")."}
				},
				"required": ["name"]
			}`),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a struct {
				Name     string `json:"name"`
				Language string `json:"language"`
			}
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if a.Name == "" {
				return invalid("name is required"), nil
			}
			lang := a.Language
			if lang == "" {
				lang = "go"
			}
			syms, err := s.WorkspaceSymbol(ctx, lang, a.Name)
			if err != nil {
				// No server (or a server-side failure): degrade to a whole-name text search.
				return grepUnverified(ctx, s.exec, `\b`+regexp.QuoteMeta(a.Name)+`\b`)
			}
			if len(syms) == 0 {
				return Outcome{Content: "no symbols found"}, nil
			}
			return Outcome{Content: formatSymbols(s.root, syms)}, nil
		},
	}
}

func referencesTool(s *Sessions) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "references",
			Description: "Find every reference to the symbol at a position, project-wide, using the language " +
				"server. Use a position from find_symbol or search. Returns each reference as path:line:column.",
			Params: positionParams(map[string]string{
				"include_declaration": "Include the symbol's own declaration in the results (default true).",
			}, "include_declaration"),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a positionArgs
			a.IncludeDeclaration = true
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if bad := a.validate(); bad != nil {
				return *bad, nil
			}
			locs, err := s.References(ctx, a.Path, a.line0(), a.char0(), a.IncludeDeclaration)
			if err != nil {
				return degradeIdentifier(ctx, s, a.Path, a.Line, a.Character)
			}
			return locationsOutcome(s.root, locs), nil
		},
	}
}

func definitionTool(s *Sessions) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "definition",
			Description: "Jump to where the symbol at a position is defined, using the language server. Use a " +
				"position from find_symbol or search. Returns the definition site as path:line:column.",
			Params: positionParams(nil),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a positionArgs
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if bad := a.validate(); bad != nil {
				return *bad, nil
			}
			locs, err := s.Definition(ctx, a.Path, a.line0(), a.char0())
			if err != nil {
				return degradeIdentifier(ctx, s, a.Path, a.Line, a.Character)
			}
			return locationsOutcome(s.root, locs), nil
		},
	}
}

func implementationTool(s *Sessions) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "implementation",
			Description: "Find implementations of the interface/method at a position (and, for a concrete type, " +
				"the interfaces it satisfies), using the language server. Returns each as path:line:column.",
			Params: positionParams(nil),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a positionArgs
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if bad := a.validate(); bad != nil {
				return *bad, nil
			}
			locs, err := s.Implementation(ctx, a.Path, a.line0(), a.char0())
			if err != nil {
				return degradeIdentifier(ctx, s, a.Path, a.Line, a.Character)
			}
			return locationsOutcome(s.root, locs), nil
		},
	}
}

func hoverTool(s *Sessions) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "hover",
			Description: "Show the type, signature, and documentation of the symbol at a position, using the " +
				"language server. Use a position from find_symbol or search.",
			Params: positionParams(nil),
		},
		fn: func(ctx context.Context, args json.RawMessage) (Outcome, error) {
			var a positionArgs
			if bad := decodeArgs(args, &a); bad != nil {
				return *bad, nil
			}
			if bad := a.validate(); bad != nil {
				return *bad, nil
			}
			h, err := s.Hover(ctx, a.Path, a.line0(), a.char0())
			if err != nil {
				// Hover has no real text equivalent; the honest floor is to show where the
				// identifier occurs so the model can read it itself.
				return degradeIdentifier(ctx, s, a.Path, a.Line, a.Character)
			}
			text := strings.TrimSpace(h.Text())
			if text == "" {
				return Outcome{Content: "no hover information"}, nil
			}
			return Outcome{Content: text}, nil
		},
	}
}

func diagnosticsTool(s *Sessions) Tool {
	return funcTool{
		def: model.ToolDef{
			Name: "diagnostics",
			Description: "Report the language server's compile/type diagnostics for a file (errors, warnings) " +
				"as path:line:column: severity: message. More precise than reading build output for a single file.",
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
			diags, err := s.Diagnostics(ctx, a.Path)
			if err != nil {
				// No language server and no text equivalent for type-checking; tell the model
				// to use `run` (build/test) instead of pretending a grep would do. The error is
				// deliberately not propagated — a missing server is a degrade, not a fatal fault
				// (specs/components/agent.md "reads degrade silently").
				//nolint:nilerr // intentional degrade: no semantic session is not an invocation error
				return Outcome{Content: "[unverified] no language server available for this file; run the build or tests with `run` to surface compile errors"}, nil
			}
			if len(diags) == 0 {
				return Outcome{Content: "no diagnostics"}, nil
			}
			return Outcome{Content: formatDiagnostics(a.Path, diags)}, nil
		},
	}
}

// --- shared position argument handling --------------------------------------------

// positionArgs is the argument shape of the file-anchored read tools (references,
// definition, implementation, hover): a worktree-relative path plus a 1-based position.
type positionArgs struct {
	Path               string `json:"path"`
	Line               int    `json:"line"`
	Character          int    `json:"character"`
	IncludeDeclaration bool   `json:"include_declaration"`
}

func (a positionArgs) validate() *Outcome {
	switch {
	case a.Path == "":
		o := invalid("path is required")
		return &o
	case a.Line < 1:
		o := invalid("line is required (1-based, as shown by find_symbol or search)")
		return &o
	case a.Character < 1:
		o := invalid("character is required (1-based column, as shown by find_symbol)")
		return &o
	}
	return nil
}

func (a positionArgs) line0() int { return a.Line - 1 }
func (a positionArgs) char0() int { return a.Character - 1 }

// positionParams builds the JSON schema for a position tool, optionally merging extra
// properties (and marking some required) so references can add include_declaration.
func positionParams(extra map[string]string, alsoRequired ...string) json.RawMessage {
	props := map[string]any{
		"path":      map[string]string{"type": "string", "description": "File path relative to the worktree root."},
		"line":      map[string]string{"type": "integer", "description": "1-based line of the symbol (from find_symbol or search)."},
		"character": map[string]string{"type": "integer", "description": "1-based column of the symbol (from find_symbol)."},
	}
	for name, desc := range extra {
		props[name] = map[string]string{"type": "boolean", "description": desc}
	}
	schema := map[string]any{
		"type":       "object",
		"properties": props,
		"required":   append([]string{"path", "line", "character"}, alsoRequired...),
	}
	b, _ := json.Marshal(schema)
	return b
}

// --- text-floor fallback ----------------------------------------------------------

// degradeIdentifier is the silent read fallback when no language server can serve a file:
// it reads the file, extracts the identifier at the position, and greps for it project-wide,
// labeling the result unverified. If the file or identifier can't be resolved it reports an
// (recoverable) error rather than guessing.
func degradeIdentifier(ctx context.Context, s *Sessions, relPath string, line, char int) (Outcome, error) {
	content, err := s.readFile(ctx, relPath)
	if err != nil {
		return invalid(fmt.Sprintf("no language server available and could not read %s for a text fallback: %v", relPath, err)), nil
	}
	ident := identifierAt(content, line, char)
	if ident == "" {
		return invalid(fmt.Sprintf("no language server available and no identifier found at %s:%d:%d for a text fallback", relPath, line, char)), nil
	}
	return grepUnverified(ctx, s.exec, `\b`+regexp.QuoteMeta(ident)+`\b`)
}

// grepUnverified runs the text-floor grep that backs every degraded read, prefixing the
// output with an explicit unverified banner so the model never mistakes a text match for a
// semantic result. A grep that cannot run at all is fatal (the sandbox is broken); a grep
// that found nothing is a normal, non-error outcome.
func grepUnverified(ctx context.Context, sb sandbox.Sandbox, pattern string) (Outcome, error) {
	res, err := sb.Exec(ctx, sandbox.Command{Path: "grep", Args: []string{"-rnE", "--", pattern, "."}})
	if err != nil {
		return Outcome{}, fmt.Errorf("agent: semantic-fallback search: %w", err)
	}
	const banner = "[unverified: no language server available; showing text matches]\n"
	if res.ExitCode == 1 && len(res.Stderr) == 0 {
		return Outcome{Content: banner + "no matches"}, nil
	}
	out := formatExec(res)
	out.Content = banner + out.Content
	return out, nil
}

// identifierAt returns the identifier token covering the 1-based (line,col) in content, or
// "" if there is none there. It treats a position one past the token's end as inside it (a
// caret at the trailing edge), matching how editors place the cursor.
func identifierAt(content string, line, col int) string {
	lines := strings.Split(content, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	row := []rune(lines[line-1])
	idx := col - 1
	if idx < 0 || idx > len(row) {
		return ""
	}
	isIdent := func(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }
	if idx == len(row) || !isIdent(row[idx]) {
		if idx > 0 && isIdent(row[idx-1]) {
			idx--
		} else {
			return ""
		}
	}
	start, end := idx, idx
	for start > 0 && isIdent(row[start-1]) {
		start--
	}
	for end < len(row) && isIdent(row[end]) {
		end++
	}
	return string(row[start:end])
}

// --- formatting -------------------------------------------------------------------

// locationsOutcome renders a set of LSP locations as the model sees them, or a plain "no
// results" when the server found none (an empty result is not an error).
func locationsOutcome(root string, locs []lsp.Location) Outcome {
	if len(locs) == 0 {
		return Outcome{Content: "no results"}
	}
	return Outcome{Content: formatLocations(root, locs)}
}

func formatLocations(root string, locs []lsp.Location) string {
	var b strings.Builder
	for _, l := range locs {
		fmt.Fprintf(&b, "%s:%d:%d\n", relForURI(root, l.URI), l.Range.Start.Line+1, l.Range.Start.Character+1)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatSymbols(root string, syms []lsp.Symbol) string {
	var b strings.Builder
	for _, s := range syms {
		loc := fmt.Sprintf("%s:%d:%d", relForURI(root, s.Location.URI), s.Location.Range.Start.Line+1, s.Location.Range.Start.Character+1)
		if s.Detail != "" {
			fmt.Fprintf(&b, "%s %s — %s (%s)\n", symbolKindName(s.Kind), s.Name, loc, s.Detail)
		} else {
			fmt.Fprintf(&b, "%s %s — %s\n", symbolKindName(s.Kind), s.Name, loc)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatDiagnostics(rel string, ds []lsp.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		fmt.Fprintf(&b, "%s:%d:%d: %s: %s", rel, d.Range.Start.Line+1, d.Range.Start.Character+1, severityName(d.Severity), d.Message)
		if d.Source != "" {
			fmt.Fprintf(&b, " [%s]", d.Source)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// relForURI turns a file:// URI back into a worktree-relative path for display. The session
// builds URIs as file://<root>/<rel>, so stripping the scheme and root recovers <rel>; an
// unexpected shape falls back to the bare path so a location is never lost.
func relForURI(root, uri string) string {
	p := strings.TrimPrefix(uri, "file://")
	if root != "" {
		if rel := strings.TrimPrefix(p, strings.TrimSuffix(root, "/")+"/"); rel != p {
			return rel
		}
	}
	return p
}

func severityName(s int) string {
	switch s {
	case 1:
		return "error"
	case 2:
		return "warning"
	case 3:
		return "info"
	case 4:
		return "hint"
	default:
		return "diagnostic"
	}
}

// symbolKindName maps the LSP SymbolKind enum (1..26) to a human label for find_symbol
// output. An unknown kind degrades to "Symbol" rather than printing a bare number.
func symbolKindName(k int) string {
	if name, ok := symbolKindNames[k]; ok {
		return name
	}
	return "Symbol"
}

var symbolKindNames = map[int]string{
	1: "File", 2: "Module", 3: "Namespace", 4: "Package", 5: "Class", 6: "Method",
	7: "Property", 8: "Field", 9: "Constructor", 10: "Enum", 11: "Interface", 12: "Function",
	13: "Variable", 14: "Constant", 15: "String", 16: "Number", 17: "Boolean", 18: "Array",
	19: "Object", 20: "Key", 21: "Null", 22: "EnumMember", 23: "Struct", 24: "Event",
	25: "Operator", 26: "TypeParameter",
}
