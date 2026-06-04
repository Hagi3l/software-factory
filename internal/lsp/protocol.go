package lsp

import (
	"context"
	"encoding/json"
	"fmt"
)

// --- LSP data types (the minimal subset the semantic tools consume) ---------------

// Position is a zero-based line/character offset (LSP characters are UTF-16 code units;
// the tool layer is responsible for any 1-based <-> 0-based translation it presents).
type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// Range is a half-open span between two Positions.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// Location is a range within a document, identified by URI.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

// locationLink is the LSP LocationLink shape some servers return for definition/
// implementation; we normalize it down to a Location via targetUri/targetRange.
type locationLink struct {
	TargetURI   string `json:"targetUri"`
	TargetRange Range  `json:"targetRange"`
}

// Diagnostic is one compile/type problem the server published for a document.
type Diagnostic struct {
	Range    Range           `json:"range"`
	Severity int             `json:"severity"` // 1=error 2=warning 3=info 4=hint
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

// Symbol is a flattened symbol (from either documentSymbol's hierarchy or
// workspace/symbol's flat list) — name, kind, and where it lives.
type Symbol struct {
	Name     string   `json:"name"`
	Kind     int      `json:"kind"`
	Location Location `json:"location"`
	Detail   string   `json:"detail,omitempty"`
}

// TextEdit is a single replacement within a document.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit is a set of edits across documents (the result of a rename or a code
// action). Both the legacy `changes` map and the newer `documentChanges` array are
// captured; Files() flattens whichever the server used into URI->edits.
type WorkspaceEdit struct {
	Changes         map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges []documentChange      `json:"documentChanges,omitempty"`
}

type documentChange struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Edits []TextEdit `json:"edits"`
}

// Files returns the edits keyed by document URI, merging the two representations a
// server may use so callers never branch on the wire shape.
func (w WorkspaceEdit) Files() map[string][]TextEdit {
	out := make(map[string][]TextEdit)
	for uri, edits := range w.Changes {
		out[uri] = append(out[uri], edits...)
	}
	for _, dc := range w.DocumentChanges {
		out[dc.TextDocument.URI] = append(out[dc.TextDocument.URI], dc.Edits...)
	}
	return out
}

// CodeAction is one server-offered fix (organize imports, quickfix, extract). Edit is
// the change it would apply; Command is its opaque command form when it has no inline
// edit (kept raw — executing server commands is out of scope for the read path).
type CodeAction struct {
	Title   string          `json:"title"`
	Kind    string          `json:"kind,omitempty"`
	Edit    *WorkspaceEdit  `json:"edit,omitempty"`
	Command json.RawMessage `json:"command,omitempty"`
}

// Hover is type/signature/doc text for a position. Contents is kept raw because LSP
// allows three shapes (MarkupContent, MarkedString, MarkedString[]); Text() flattens.
type Hover struct {
	Contents json.RawMessage `json:"contents"`
}

// Text extracts the plain-text body of a hover across the three LSP content shapes.
func (h Hover) Text() string {
	if len(h.Contents) == 0 {
		return ""
	}
	// MarkupContent: {kind, value}
	var mc struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(h.Contents, &mc); err == nil && mc.Value != "" {
		return mc.Value
	}
	// Plain string
	var s string
	if err := json.Unmarshal(h.Contents, &s); err == nil {
		return s
	}
	// MarkedString[] or {language,value}[]
	var arr []json.RawMessage
	if err := json.Unmarshal(h.Contents, &arr); err == nil {
		var out string
		for _, e := range arr {
			var ms struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(e, &ms); err == nil && ms.Value != "" {
				out += ms.Value + "\n"
				continue
			}
			var es string
			if err := json.Unmarshal(e, &es); err == nil {
				out += es + "\n"
			}
		}
		return out
	}
	return string(h.Contents)
}

// --- Lifecycle --------------------------------------------------------------------

// Initialize performs the LSP handshake: the `initialize` request with rootURI plus a
// modest capability set, then the `initialized` notification. After it returns the
// server is ready for didOpen and queries.
func (c *Client) Initialize(ctx context.Context, rootURI string) error {
	params := map[string]any{
		"processId":    nil,
		"rootUri":      rootURI,
		"capabilities": clientCapabilities,
		"workspaceFolders": []map[string]string{
			{"uri": rootURI, "name": "workspace"},
		},
	}
	if _, err := c.call(ctx, "initialize", params); err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}
	if err := c.notify("initialized", struct{}{}); err != nil {
		return fmt.Errorf("lsp: initialized: %w", err)
	}
	return nil
}

// clientCapabilities is a static, modest capability advertisement — enough for gopls to
// enable the features the tools use without negotiating dynamic registration.
var clientCapabilities = json.RawMessage(`{
	"textDocument": {
		"synchronization": {"didSave": true, "dynamicRegistration": false},
		"definition": {"dynamicRegistration": false},
		"references": {"dynamicRegistration": false},
		"implementation": {"dynamicRegistration": false},
		"hover": {"dynamicRegistration": false, "contentFormat": ["plaintext", "markdown"]},
		"documentSymbol": {"dynamicRegistration": false, "hierarchicalDocumentSymbolSupport": true},
		"publishDiagnostics": {},
		"rename": {"dynamicRegistration": false},
		"codeAction": {"dynamicRegistration": false}
	},
	"workspace": {
		"symbol": {"dynamicRegistration": false},
		"configuration": true,
		"workspaceFolders": true
	}
}`)

// Shutdown sends the orderly `shutdown` request followed by the `exit` notification.
// Best-effort: a server that already died returns an error the caller can ignore, since
// Close (and the sandbox teardown) is the real backstop.
func (c *Client) Shutdown(ctx context.Context) error {
	if _, err := c.call(ctx, "shutdown", nil); err != nil {
		return err
	}
	return c.notify("exit", nil)
}

// --- Document synchronization -----------------------------------------------------

// DidOpen tells the server a document is open with the given in-memory text. The
// server's view is this overlay, not the file on disk, so the text must be current.
func (c *Client) DidOpen(uri, languageID, text string, version int) error {
	return c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    version,
			"text":       text,
		},
	})
}

// DidChange replaces a document's text wholesale (full-sync). version must increase.
func (c *Client) DidChange(uri, text string, version int) error {
	return c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": uri, "version": version},
		"contentChanges": []map[string]any{{"text": text}},
	})
}

// DidSave notifies the server a document was saved.
func (c *Client) DidSave(uri string) error {
	return c.notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]string{"uri": uri},
	})
}

// --- Queries ----------------------------------------------------------------------

func posParams(uri string, line, char int) map[string]any {
	return map[string]any{
		"textDocument": map[string]string{"uri": uri},
		"position":     map[string]int{"line": line, "character": char},
	}
}

// Definition returns where the symbol at the position is defined.
func (c *Client) Definition(ctx context.Context, uri string, line, char int) ([]Location, error) {
	raw, err := c.call(ctx, "textDocument/definition", posParams(uri, line, char))
	if err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// Implementation returns implementations of the interface/method at the position.
func (c *Client) Implementation(ctx context.Context, uri string, line, char int) ([]Location, error) {
	raw, err := c.call(ctx, "textDocument/implementation", posParams(uri, line, char))
	if err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// References returns all references to the symbol at the position. includeDecl controls
// whether the declaration itself is included.
func (c *Client) References(ctx context.Context, uri string, line, char int, includeDecl bool) ([]Location, error) {
	params := posParams(uri, line, char)
	params["context"] = map[string]bool{"includeDeclaration": includeDecl}
	raw, err := c.call(ctx, "textDocument/references", params)
	if err != nil {
		return nil, err
	}
	return parseLocations(raw), nil
}

// Hover returns type/signature/doc text for the position.
func (c *Client) Hover(ctx context.Context, uri string, line, char int) (Hover, error) {
	raw, err := c.call(ctx, "textDocument/hover", posParams(uri, line, char))
	if err != nil {
		return Hover{}, err
	}
	var h Hover
	if len(raw) == 0 || string(raw) == "null" {
		return Hover{}, nil
	}
	if err := json.Unmarshal(raw, &h); err != nil {
		return Hover{}, fmt.Errorf("lsp: decode hover: %w", err)
	}
	return h, nil
}

// DocumentSymbol returns the symbols declared in a document, flattened from either the
// hierarchical DocumentSymbol[] or the flat SymbolInformation[] the server may send.
func (c *Client) DocumentSymbol(ctx context.Context, uri string) ([]Symbol, error) {
	raw, err := c.call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]string{"uri": uri},
	})
	if err != nil {
		return nil, err
	}
	return parseDocumentSymbols(raw, uri), nil
}

// WorkspaceSymbol returns project-wide symbols matching the query (the engine behind
// find_symbol — a symbol by name with no path).
func (c *Client) WorkspaceSymbol(ctx context.Context, query string) ([]Symbol, error) {
	raw, err := c.call(ctx, "workspace/symbol", map[string]string{"query": query})
	if err != nil {
		return nil, err
	}
	var infos []struct {
		Name     string   `json:"name"`
		Kind     int      `json:"kind"`
		Location Location `json:"location"`
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &infos); err != nil {
		return nil, fmt.Errorf("lsp: decode workspace/symbol: %w", err)
	}
	out := make([]Symbol, len(infos))
	for i, s := range infos {
		out[i] = Symbol{Name: s.Name, Kind: s.Kind, Location: s.Location}
	}
	return out, nil
}

// Rename computes the project-wide edits to rename the symbol at the position to
// newName. It returns the WorkspaceEdit; applying it to the worktree is the caller's
// job (the transformation tool, T6.3), which keeps producer/applier separable.
func (c *Client) Rename(ctx context.Context, uri string, line, char int, newName string) (WorkspaceEdit, error) {
	params := posParams(uri, line, char)
	params["newName"] = newName
	raw, err := c.call(ctx, "textDocument/rename", params)
	if err != nil {
		return WorkspaceEdit{}, err
	}
	var we WorkspaceEdit
	if len(raw) == 0 || string(raw) == "null" {
		return WorkspaceEdit{}, nil
	}
	if err := json.Unmarshal(raw, &we); err != nil {
		return WorkspaceEdit{}, fmt.Errorf("lsp: decode rename: %w", err)
	}
	return we, nil
}

// CodeAction returns the server's offered actions for a range (organize imports,
// quickfix, extract). Each carries either an inline WorkspaceEdit or an opaque command.
func (c *Client) CodeAction(ctx context.Context, uri string, rng Range) ([]CodeAction, error) {
	raw, err := c.call(ctx, "textDocument/codeAction", map[string]any{
		"textDocument": map[string]string{"uri": uri},
		"range":        rng,
		"context":      map[string]any{"diagnostics": []any{}},
	})
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var actions []CodeAction
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, fmt.Errorf("lsp: decode codeAction: %w", err)
	}
	return actions, nil
}

// Diagnostics returns the diagnostics the server has published for a document. Because
// publishing is asynchronous (the server pushes them after a didOpen/didChange), it
// blocks for the first batch for this URI if none has arrived yet, bounded by ctx.
func (c *Client) Diagnostics(ctx context.Context, uri string) ([]Diagnostic, error) {
	c.mu.Lock()
	if d, ok := c.diags[uri]; ok {
		c.mu.Unlock()
		return d, nil
	}
	if c.closed {
		c.mu.Unlock()
		return nil, c.errLocked()
	}
	wait := make(chan struct{})
	c.diagW[uri] = append(c.diagW[uri], wait)
	c.mu.Unlock()

	select {
	case <-wait:
		c.mu.Lock()
		d := c.diags[uri]
		c.mu.Unlock()
		return d, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.errLocked()
	}
}

// --- result normalization ---------------------------------------------------------

// parseLocations normalizes the three shapes a location-returning request may yield:
// a single Location, a Location[], or a LocationLink[].
func parseLocations(raw json.RawMessage) []Location {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var arr []Location
	if err := json.Unmarshal(raw, &arr); err == nil && allHaveURI(arr) {
		return arr
	}
	var one Location
	if err := json.Unmarshal(raw, &one); err == nil && one.URI != "" {
		return []Location{one}
	}
	var links []locationLink
	if err := json.Unmarshal(raw, &links); err == nil {
		out := make([]Location, 0, len(links))
		for _, l := range links {
			if l.TargetURI != "" {
				out = append(out, Location{URI: l.TargetURI, Range: l.TargetRange})
			}
		}
		return out
	}
	return nil
}

func allHaveURI(locs []Location) bool {
	for _, l := range locs {
		if l.URI == "" {
			return false
		}
	}
	return len(locs) > 0
}

// documentSymbol is the hierarchical shape; children recurse.
type documentSymbol struct {
	Name     string           `json:"name"`
	Detail   string           `json:"detail"`
	Kind     int              `json:"kind"`
	Range    Range            `json:"range"`
	Children []documentSymbol `json:"children"`
}

// parseDocumentSymbols flattens either documentSymbol[] (hierarchical, range-based) or
// SymbolInformation[] (flat, location-based) into one []Symbol.
func parseDocumentSymbols(raw json.RawMessage, uri string) []Symbol {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Try hierarchical first (has "selectionRange"/"children"; "range" but no "location").
	var hier []documentSymbol
	if err := json.Unmarshal(raw, &hier); err == nil && looksHierarchical(raw) {
		var out []Symbol
		var walk func(ds []documentSymbol)
		walk = func(ds []documentSymbol) {
			for _, d := range ds {
				out = append(out, Symbol{Name: d.Name, Kind: d.Kind, Detail: d.Detail, Location: Location{URI: uri, Range: d.Range}})
				walk(d.Children)
			}
		}
		walk(hier)
		return out
	}
	// SymbolInformation[]: flat, each carries its own location.
	var infos []struct {
		Name     string   `json:"name"`
		Kind     int      `json:"kind"`
		Location Location `json:"location"`
	}
	if err := json.Unmarshal(raw, &infos); err == nil {
		out := make([]Symbol, len(infos))
		for i, s := range infos {
			out[i] = Symbol{Name: s.Name, Kind: s.Kind, Location: s.Location}
		}
		return out
	}
	return nil
}

// looksHierarchical reports whether a documentSymbol result is the hierarchical shape
// (a top-level element with a "selectionRange" key, which SymbolInformation lacks).
func looksHierarchical(raw json.RawMessage) bool {
	var probe []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe) == 0 {
		return false
	}
	_, ok := probe[0]["selectionRange"]
	return ok
}
