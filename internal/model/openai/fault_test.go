package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/openai/openai-go/v3/option"

	"github.com/Loxstomper/harness/internal/model"
)

// Fault classification (T14.2, specs/models.md "Transient provider faults are absorbed
// at the relay"): the adapter — the only layer that can read its SDK's error shape —
// classifies completion errors into the canonical model.Fault the trusted relay retries
// on. Asserted against real wire responses, not synthetic errors, so the SDK's own
// error decoding is part of what's tested. option.WithMaxRetries(0) disables the SDK's
// internal connect retry so the tests assert classification, not the SDK's policy.
func TestCompleteClassifiesFaults(t *testing.T) {
	req := model.Request{Messages: []model.Message{{Role: model.RoleUser, Text: "hi"}}}
	complete := func(t *testing.T, srv *httptest.Server) error {
		t.Helper()
		a := New("m", option.WithAPIKey("test"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
		_, err := a.Complete(context.Background(), req, nil)
		if err == nil {
			t.Fatal("Complete: want error, got nil")
		}
		return err
	}
	jsonError := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		}))
	}

	t.Run("http 429 is transient", func(t *testing.T) {
		srv := jsonError(http.StatusTooManyRequests, `{"error":{"message":"slow down","type":"rate_limit_error"}}`)
		defer srv.Close()
		if err := complete(t, srv); !model.Transient(err) {
			t.Errorf("a 429 must classify transient, got terminal: %v", err)
		}
	})

	t.Run("http 500 is transient", func(t *testing.T) {
		srv := jsonError(http.StatusInternalServerError, `{"error":{"message":"boom","type":"server_error"}}`)
		defer srv.Close()
		if err := complete(t, srv); !model.Transient(err) {
			t.Errorf("a 500 must classify transient, got terminal: %v", err)
		}
	})

	t.Run("http 400 is terminal", func(t *testing.T) {
		srv := jsonError(http.StatusBadRequest, `{"error":{"message":"context overflow","type":"invalid_request_error"}}`)
		defer srv.Close()
		if err := complete(t, srv); model.Transient(err) {
			t.Errorf("a 400 must classify terminal, got transient: %v", err)
		}
	})

	t.Run("mid-stream cut is transient", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// Cut the connection mid-stream: the client sees a read error, not a status.
			panic(http.ErrAbortHandler)
		}))
		defer srv.Close()
		if err := complete(t, srv); !model.Transient(err) {
			t.Errorf("a mid-stream reset must classify transient, got terminal: %v", err)
		}
	})
}
