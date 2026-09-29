package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// rig is a running server on a real listener, so the tests exercise the actual HTTP and
// WebSocket paths rather than handlers in isolation.
type rig struct {
	t      *testing.T
	server *Server
	http   *httptest.Server
}

func newRig(t *testing.T, cfg Config) *rig {
	t.Helper()

	if cfg.SessionDir == "" {
		cfg.SessionDir = t.TempDir()
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- server.Run(ctx) }()

	ts := httptest.NewServer(server.Handler())
	t.Cleanup(func() {
		ts.Close()
		cancel()
		select {
		case err := <-stopped:
			if err != nil {
				t.Errorf("server shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop within 5s")
		}
	})

	return &rig{t: t, server: server, http: ts}
}

func newDefaultRig(t *testing.T) *rig {
	t.Helper()
	return newRig(t, Config{Symbols: []string{"NOVA", "ORBIT"}})
}

// do performs a request and returns the status and raw body.
func (r *rig) do(method, path, body string) (int, []byte) {
	r.t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, r.http.URL+path, reader)
	if err != nil {
		r.t.Fatalf("building %s %s: %v", method, path, err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := r.http.Client().Do(req)
	if err != nil {
		r.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("reading %s %s response: %v", method, path, err)
	}
	return resp.StatusCode, raw
}

// submit posts an order and expects it to be accepted.
func (r *rig) submit(symbol, body string) market.Ack {
	r.t.Helper()
	status, raw := r.do(http.MethodPost, "/api/markets/"+symbol+"/orders", body)
	if status != http.StatusCreated {
		r.t.Fatalf("submitting %s returned %d: %s", body, status, raw)
	}
	var ack market.Ack
	decodeInto(r.t, raw, &ack)
	return ack
}

// expectStatus performs a request and asserts the status code, returning the body.
func (r *rig) expectStatus(method, path, body string, want int) []byte {
	r.t.Helper()
	status, raw := r.do(method, path, body)
	if status != want {
		r.t.Fatalf("%s %s returned %d, want %d: %s", method, path, status, want, raw)
	}
	return raw
}

func (r *rig) snapshot(symbol string) engine.BookSnapshot {
	r.t.Helper()
	raw := r.expectStatus(http.MethodGet, "/api/markets/"+symbol+"/snapshot", "", http.StatusOK)
	var snap engine.BookSnapshot
	decodeInto(r.t, raw, &snap)
	return snap
}

func (r *rig) openOrders(symbol string) []engine.View {
	r.t.Helper()
	raw := r.expectStatus(http.MethodGet, "/api/markets/"+symbol+"/orders", "", http.StatusOK)
	var orders []engine.View
	decodeInto(r.t, raw, &orders)
	return orders
}

// actorSubmit sends a command straight to the market, for seeding another participant's
// orders that HTTP could never create.
func (r *rig) actorSubmit(symbol string, cmd market.Command) market.Ack {
	r.t.Helper()
	actor, err := r.server.Exchange().Market(symbol)
	if err != nil {
		r.t.Fatalf("looking up %s: %v", symbol, err)
	}
	ack, err := actor.Submit(context.Background(), cmd)
	if err != nil {
		r.t.Fatalf("submitting to %s: %v", symbol, err)
	}
	if !ack.Accepted {
		r.t.Fatalf("seeding %s was rejected: %s", symbol, ack.Rejection)
	}
	return ack
}

func (r *rig) errorMessage(raw []byte) string {
	r.t.Helper()
	var body errorResponse
	decodeInto(r.t, raw, &body)
	return body.Error
}

func decodeInto(t *testing.T, raw []byte, into any) {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(into); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}
}
