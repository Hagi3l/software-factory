// Package modeltest provides a deterministic fake model: an httptest server that
// speaks the OpenAI Chat Completions streaming wire protocol and replays a *scripted*
// sequence of responses keyed on the request count. It exists so the kernel's
// machinery — the agent ReAct loop, the broker relay, the tool contract, gating, and
// merge — can be driven end-to-end without a capable (or any) real model and without a
// network (see specs/bootstrap.md "Testing the spine without a capable model",
// specs/models.md "deterministically-fakeable").
//
// Why a real HTTP server rather than a fake model.Adapter: wiring the server through an
// `openai-compat` model entry exercises the *production* OpenAI adapter against the real
// streaming SSE format, so the test pins the actual provider wire contract, not a
// stub. Nothing in the production model layer changes — the server is selected purely by
// pointing a model entry's endpoint at it.
//
// The script is the contract: each Turn is one model response (assistant text and/or
// tool calls), and authoring it to match the real tool schema (WorkspaceTools +
// submit/escalate/request_subtask) is what makes the test a regression guard on the
// tool contract.
package modeltest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// ToolCall is one scripted tool call in a Turn. Args is the raw JSON arguments object
// the model would emit for the call; an empty Args is sent as "{}" so the wire payload
// stays valid (matching what the production adapter does for a no-argument call).
type ToolCall struct {
	ID   string
	Name string
	Args string
}

// Turn is one scripted model response. A Turn with ToolCalls finishes with reason
// "tool_calls" (the loop will dispatch them); a Turn with only Text finishes with
// "stop". Mirroring the two finish reasons the agent loop branches on is deliberate —
// the fake must drive the same control flow a real model would.
type Turn struct {
	Text      string
	ToolCalls []ToolCall
}

// Server is a fake OpenAI-compatible chat-completions endpoint replaying Turns in
// order, one per request. It is safe for concurrent requests (the agent loop is
// sequential, but the embedded NATS/broker plumbing may overlap calls across
// invocations). Construct it with NewServer; it is torn down via t.Cleanup.
type Server struct {
	srv   *httptest.Server
	t     testing.TB
	mu    sync.Mutex
	turns []Turn
	count int
}

// NewServer starts a fake model server that replays turns and registers its shutdown
// with t.Cleanup. The handler ignores the request path so it works regardless of how
// the OpenAI SDK joins the base URL with "chat/completions".
func NewServer(t testing.TB, turns []Turn) *Server {
	t.Helper()
	s := &Server{t: t, turns: turns}
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

// URL is the base URL to point an `openai-compat` model entry's endpoint at.
func (s *Server) URL() string { return s.srv.URL }

// Requests returns how many completion requests the server has served — i.e. how many
// agent turns ran. Tests assert on it to pin the expected number of model round-trips.
func (s *Server) Requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	idx := s.count
	s.count++
	var turn Turn
	overrun := idx >= len(s.turns)
	if !overrun {
		turn = s.turns[idx]
	}
	s.mu.Unlock()

	if overrun {
		// The script is exhausted: the loop made more model calls than scripted, which
		// means the control flow diverged from what the test encodes. Fail loudly and
		// return an error status so the adapter surfaces it rather than the test hanging
		// on a silent stall.
		s.t.Errorf("modeltest: unexpected model request #%d beyond a script of %d turn(s)", idx+1, len(s.turns))
		http.Error(w, "modeltest: script exhausted", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	for _, c := range s.writeChunks(turn) {
		b, err := json.Marshal(c)
		if err != nil {
			s.t.Errorf("modeltest: marshal chunk: %v", err)
			return
		}
		// SSE frame: a "data:" line then a blank line terminates the event (the SDK's
		// ssestream decoder dispatches on the empty line).
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	// The terminal sentinel the SDK's Stream.Next() stops on.
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// writeChunks renders one Turn into the streamed chunks the OpenAI accumulator folds
// back into a completion: a content/tool-call chunk carrying the finish reason, then a
// usage-only chunk (empty choices, as OpenAI sends when stream_options.include_usage is
// set — which the production adapter always sets). All chunks share one id, which the
// accumulator requires.
func (s *Server) writeChunks(turn Turn) []chunk {
	const id = "modeltest-cmpl"
	finish := "stop"
	delta := chunkDelta{Role: "assistant", Content: turn.Text}
	if len(turn.ToolCalls) > 0 {
		finish = "tool_calls"
		for i, tc := range turn.ToolCalls {
			args := tc.Args
			if args == "" {
				args = "{}"
			}
			delta.ToolCalls = append(delta.ToolCalls, chunkToolCall{
				Index:    i,
				ID:       tc.ID,
				Type:     "function",
				Function: chunkToolFunc{Name: tc.Name, Arguments: args},
			})
		}
	}
	return []chunk{
		{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: 1700000000,
			Model:   "fake",
			Choices: []chunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		},
		{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: 1700000000,
			Model:   "fake",
			Choices: []chunkChoice{},
			Usage:   &chunkUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
	}
}

// The chunk types below are a minimal, hand-rolled encoding of the OpenAI
// ChatCompletionChunk wire shape — only the fields the SDK accumulator reads, with the
// exact JSON keys it decodes. Defining them here (rather than marshaling the SDK's
// decode-oriented response structs) keeps full control over the bytes on the wire.
type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chunkUsage   `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason string     `json:"finish_reason,omitempty"`
}

type chunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []chunkToolCall `json:"tool_calls,omitempty"`
}

type chunkToolCall struct {
	Index    int           `json:"index"`
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function chunkToolFunc `json:"function"`
}

type chunkToolFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chunkUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
