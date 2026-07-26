// Package lsp is a minimal Language Server Protocol client: JSON-RPC 2.0 over the LSP
// `Content-Length` stdio framing, with just the methods the agent's semantic tools
// need (Phase 6). It is a stdlib-only leaf — it speaks to *any* language server over a
// pair of streams (an in-sandbox gopls reached through sandbox.SessionStream, or an
// in-memory fake in tests) and knows nothing about sandboxes, the worktree, or the
// manifest. That separation is deliberate: the protocol transport is generic and
// reusable; the session manager (internal/agent) owns the sandbox/worktree specifics,
// the same canonical-interface / thin-adapter split the model layer uses (specs/
// components/agent.md "Semantic tools (LSP-backed)").
package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
)

// ErrClosed is returned by calls made after the client (or its underlying transport)
// has shut down. A dead language server surfaces as this once its stream hits EOF.
var ErrClosed = errors.New("lsp: client closed")

// Client is a JSON-RPC 2.0 client speaking LSP over a stdio stream pair. It is safe for
// concurrent use; a single background goroutine reads the server stream and demuxes
// responses (by id) from notifications (publishDiagnostics is cached; server→client
// requests are answered with defaults so the server never stalls).
type Client struct {
	w      io.Writer // to the server (its stdin)
	closer io.Closer // closes the whole transport on shutdown
	log    *slog.Logger

	wmu sync.Mutex // serializes frame writes

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rpcResult
	diags   map[string][]Diagnostic    // latest publishDiagnostics per document URI
	diagW   map[string][]chan struct{} // waiters blocked on the first diagnostics for a URI
	closed  bool
	failErr error
	done    chan struct{} // closed once on shutdown/transport failure
}

type rpcResult struct {
	result json.RawMessage
	err    *rpcError
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp: rpc error %d: %s", e.Code, e.Message) }

// New builds a Client over a server's stdout (read) and stdin (write) plus a closer for
// the whole transport. It starts the reader goroutine immediately. A nil logger discards.
func New(stdout io.Reader, stdin io.Writer, closer io.Closer, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	c := &Client{
		w:       stdin,
		closer:  closer,
		log:     log,
		nextID:  1,
		pending: make(map[int]chan rpcResult),
		diags:   make(map[string][]Diagnostic),
		diagW:   make(map[string][]chan struct{}),
		done:    make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c
}

// call sends a request and blocks for the matching response, the context, or transport
// death. params is marshaled as the JSON-RPC params; a nil result returns nil bytes.
func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ch := make(chan rpcResult, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.errLocked()
	}
	id := c.nextID
	c.nextID++
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.writeMessage(outbound{Version: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("lsp: write %s: %w", method, err)
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		return res.result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, c.errLocked()
	}
}

// notify sends a notification (no id, no response).
func (c *Client) notify(method string, params any) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return c.errLocked()
	}
	if err := c.writeMessage(outbound{Version: "2.0", Method: method, Params: params}); err != nil {
		return fmt.Errorf("lsp: notify %s: %w", method, err)
	}
	return nil
}

// outbound is a request or notification we send (id omitted => notification).
type outbound struct {
	Version string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func (c *Client) writeMessage(m outbound) error {
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

// readLoop reads framed messages until the stream ends, dispatching each. On any read
// error (EOF when the server exits or the sandbox is torn down) it fails the client,
// unblocking every pending call with ErrClosed.
func (c *Client) readLoop(r io.Reader) {
	br := bufio.NewReader(r)
	for {
		payload, err := readFrame(br)
		if err != nil {
			c.fail(err)
			return
		}
		c.dispatch(payload)
	}
}

// inbound is the superset envelope for anything the server sends: a response (id +
// result/error), a notification (method, no id), or a server→client request (method +
// id, which we must answer).
type inbound struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Result json.RawMessage  `json:"result"`
	Error  *rpcError        `json:"error"`
	Params json.RawMessage  `json:"params"`
}

func (c *Client) dispatch(payload []byte) {
	var in inbound
	if err := json.Unmarshal(payload, &in); err != nil {
		c.log.DebugContext(context.Background(), "lsp: undecodable message", "err", err)
		return
	}
	switch {
	case in.Method != "" && in.ID != nil:
		// Server→client request: answer with a default so the server never blocks.
		c.answerServerRequest(*in.ID, in.Method, in.Params)
	case in.Method != "":
		c.handleNotification(in.Method, in.Params)
	case in.ID != nil:
		c.deliverResponse(*in.ID, rpcResult{result: in.Result, err: in.Error})
	default:
		c.log.DebugContext(context.Background(), "lsp: ignoring message with neither method nor id")
	}
}

func (c *Client) deliverResponse(idRaw json.RawMessage, res rpcResult) {
	id, err := strconv.Atoi(strings.TrimSpace(string(idRaw)))
	if err != nil {
		c.log.DebugContext(context.Background(), "lsp: response with non-integer id", "id", string(idRaw))
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- res
	}
}

// answerServerRequest replies to the handful of requests a language server makes back
// to the client. We do not implement their semantics — we answer with safe defaults so
// the server proceeds: an array of nulls for workspace/configuration (each item falls
// back to the server's defaults), null for everything else (registerCapability,
// workDoneProgress/create, showMessageRequest, …).
func (c *Client) answerServerRequest(idRaw json.RawMessage, method string, params json.RawMessage) {
	var result any
	if method == "workspace/configuration" {
		var p struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(params, &p)
		result = make([]any, len(p.Items)) // []null of the right length
	}
	reply := struct {
		Version string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{Version: "2.0", ID: idRaw, Result: result}
	body, err := json.Marshal(reply)
	if err != nil {
		return
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return
	}
	_, _ = c.w.Write(body)
}

func (c *Client) handleNotification(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return // window/logMessage, $/progress, etc. — not needed by the tools
	}
	var p struct {
		URI         string       `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	c.mu.Lock()
	c.diags[p.URI] = p.Diagnostics
	waiters := c.diagW[p.URI]
	delete(c.diagW, p.URI)
	c.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

// fail records the first transport error and unblocks everything waiting. Idempotent.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.failErr = err
		close(c.done)
	}
	c.mu.Unlock()
}

// errLocked returns the reason the client is unusable: the transport failure if one was
// recorded, else ErrClosed (a clean Close).
func (c *Client) errLocked() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failErr != nil && !errors.Is(c.failErr, io.EOF) {
		return fmt.Errorf("%w: %v", ErrClosed, c.failErr)
	}
	return ErrClosed
}

// Close shuts the client and the underlying transport down. It is idempotent and safe
// to call concurrently with in-flight calls (they unblock with ErrClosed).
func (c *Client) Close() error {
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		close(c.done)
	}
	c.mu.Unlock()
	if c.closer != nil {
		return c.closer.Close()
	}
	return nil
}

// readFrame reads one LSP message: `Content-Length: N\r\n` headers, a blank line, then
// exactly N body bytes. Other headers (Content-Type) are tolerated and ignored.
func readFrame(br *bufio.Reader) ([]byte, error) {
	var length int
	haveLength := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q: %w", value, err)
			}
			length = n
			haveLength = true
		}
	}
	if !haveLength {
		return nil, errors.New("lsp: message without Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return nil, err
	}
	return body, nil
}
