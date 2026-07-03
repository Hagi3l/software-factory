package anthropic

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Loxstomper/harness/internal/model"
)

// Fault classification (T14.2, specs/models.md "Transient provider faults are absorbed
// at the relay"), asserted against real wire responses so the SDK's error decoding is
// part of what's tested. Anthropic has the extra mid-stream shape: an in-band
// `event: error` SSE frame decodes into a typed error stamped with the ORIGINAL
// response's status (200 — the stream had opened), so there the error *type* carries
// the class. option.WithMaxRetries(0) disables the SDK's internal connect retry so the
// tests assert classification, not the SDK's policy.
func TestCompleteClassifiesFaults(t *testing.T) {
	req := model.Request{MaxTokens: 16, Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}}
	complete := func(t *testing.T, srv *httptest.Server) error {
		t.Helper()
		a := New("claude-test", option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
		_, err := a.Complete(context.Background(), req, nil)
		if err == nil {
			t.Fatal("Complete: want error, got nil")
		}
		return err
	}
	jsonError := func(status int, errType string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":"nope"}}`, errType)
		}))
	}
	// streamThenError opens a healthy stream (message_start carries the billed input
	// tokens) and then fails it with an in-band error frame of the given type.
	streamThenError := func(errType string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: message_start\n")
			fmt.Fprint(w, `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":42,"output_tokens":1}}}`+"\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			fmt.Fprint(w, "event: error\n")
			fmt.Fprintf(w, `data: {"type":"error","error":{"type":%q,"message":"nope"}}`+"\n\n", errType)
		}))
	}

	t.Run("http 429 is transient", func(t *testing.T) {
		srv := jsonError(http.StatusTooManyRequests, "rate_limit_error")
		defer srv.Close()
		if err := complete(t, srv); !model.Transient(err) {
			t.Errorf("a 429 must classify transient, got terminal: %v", err)
		}
	})

	t.Run("http 400 is terminal", func(t *testing.T) {
		srv := jsonError(http.StatusBadRequest, "invalid_request_error")
		defer srv.Close()
		if err := complete(t, srv); model.Transient(err) {
			t.Errorf("a 400 must classify terminal, got transient: %v", err)
		}
	})

	t.Run("mid-stream overloaded is transient and carries billed usage", func(t *testing.T) {
		srv := streamThenError("overloaded_error")
		defer srv.Close()
		err := complete(t, srv)
		if !model.Transient(err) {
			t.Errorf("a mid-stream overloaded_error must classify transient, got terminal: %v", err)
		}
		// message_start already billed the input tokens — the fault must carry them so
		// the relay draws the budget for the failed attempt.
		if u, ok := model.FaultUsage(err); !ok || u.InputTokens != 42 {
			t.Errorf("FaultUsage = %+v %v, want InputTokens:42 from the partial accumulator", u, ok)
		}
	})

	t.Run("mid-stream invalid_request is terminal", func(t *testing.T) {
		srv := streamThenError("invalid_request_error")
		defer srv.Close()
		if err := complete(t, srv); model.Transient(err) {
			t.Errorf("a mid-stream invalid_request_error must classify terminal, got transient: %v", err)
		}
	})
}
