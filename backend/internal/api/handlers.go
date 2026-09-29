package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/instrument"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

// engineUnknownOrder is aliased so statusFor can classify it without importing engine
// into server.go purely for one sentinel.
var engineUnknownOrder = engine.ErrUnknownOrder

// maxBodyBytes caps a request body. Order submissions are tiny; anything larger is either
// a mistake or an attempt to make the server allocate.
const maxBodyBytes = 4 << 10

// orderRequest is the submission schema.
//
// There is deliberately no participant field. The identity is assigned by the server, and
// unknown fields are rejected, so a client that tries to name itself gets a clear error
// rather than silently being ignored.
type orderRequest struct {
	Side     engine.Side       `json:"side"`
	Type     engine.OrderType  `json:"type"`
	Price    engine.PriceTicks `json:"price,omitempty"`
	Quantity int64             `json:"quantity"`
}

type errorResponse struct {
	Error    string `json:"error"`
	Sequence uint64 `json:"sequence,omitempty"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "ok",
		"markets":     s.exchange.Symbols(),
		"participant": s.cfg.Participant,
	})
}

func (s *Server) handleMarkets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, instrument.All())
}

func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	actor, err := s.exchange.Market(r.PathValue("symbol"))
	if err != nil {
		writeError(w, err)
		return
	}
	snap, err := actor.Snapshot(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) handleTrades(w http.ResponseWriter, r *http.Request) {
	actor, err := s.exchange.Market(r.PathValue("symbol"))
	if err != nil {
		writeError(w, err)
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeStatus(w, http.StatusBadRequest, fmt.Sprintf("limit must be a non-negative integer, got %q", raw))
			return
		}
		limit = n
	}

	trades, err := actor.RecentTrades(r.Context(), limit)
	if err != nil {
		writeError(w, err)
		return
	}
	if trades == nil {
		trades = []engine.Trade{}
	}
	writeJSON(w, http.StatusOK, trades)
}

// handleOpenOrders returns the resting orders belonging to the server-assigned
// participant. A client cannot ask about anyone else's.
func (s *Server) handleOpenOrders(w http.ResponseWriter, r *http.Request) {
	actor, err := s.exchange.Market(r.PathValue("symbol"))
	if err != nil {
		writeError(w, err)
		return
	}
	orders, err := actor.OpenOrders(r.Context(), s.cfg.Participant)
	if err != nil {
		writeError(w, err)
		return
	}
	if orders == nil {
		orders = []engine.View{}
	}
	writeJSON(w, http.StatusOK, orders)
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	actor, err := s.exchange.Market(r.PathValue("symbol"))
	if err != nil {
		writeError(w, err)
		return
	}

	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	// A client that sends "participant" is trying to choose its identity. Saying so is
	// better than accepting the request and quietly doing something else.
	dec.DisallowUnknownFields()

	var req orderRequest
	if err := dec.Decode(&req); err != nil {
		writeStatus(w, http.StatusBadRequest, decodeMessage(err))
		return
	}

	var kind market.Kind
	switch req.Type {
	case engine.Limit:
		kind = market.SubmitLimit
	case engine.Market:
		kind = market.SubmitMarket
	default:
		writeStatus(w, http.StatusBadRequest, "type must be limit or market")
		return
	}

	// Built as a raw command rather than through the helpers, so the market's own
	// validation is the single place the rules live. A market order carrying a price, for
	// instance, is rejected there rather than being silently stripped here.
	cmd := market.Command{
		Kind:          kind,
		ParticipantID: s.cfg.Participant,
		Side:          req.Side,
		Quantity:      req.Quantity,
		LimitPrice:    req.Price,
	}

	ack, err := actor.Submit(r.Context(), cmd)
	if err != nil {
		writeError(w, err)
		return
	}
	if !ack.Accepted {
		writeJSON(w, statusFor(ack.Err), errorResponse{Error: ack.Rejection, Sequence: ack.Sequence})
		return
	}
	writeJSON(w, http.StatusCreated, ack)
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	actor, err := s.exchange.Market(r.PathValue("symbol"))
	if err != nil {
		writeError(w, err)
		return
	}

	raw := r.PathValue("id")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		writeStatus(w, http.StatusBadRequest, fmt.Sprintf("order id must be a positive integer, got %q", raw))
		return
	}

	// The participant is the server's, not the client's, so the market's ownership check
	// is what stops a browser cancelling a bot's quote. It answers 403 rather than
	// pretending the order does not exist.
	ack, err := actor.Submit(r.Context(), market.CancelOrder(s.cfg.Participant, engine.OrderID(id)))
	if err != nil {
		writeError(w, err)
		return
	}
	if !ack.Accepted {
		writeJSON(w, statusFor(ack.Err), errorResponse{Error: ack.Rejection, Sequence: ack.Sequence})
		return
	}
	writeJSON(w, http.StatusOK, ack)
}

// decodeMessage turns a JSON decode failure into something a client can act on.
func decodeMessage(err error) string {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return "request body is too large"
	}
	if errors.Is(err, io.EOF) {
		return "request body is empty"
	}
	return err.Error()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// A failure here means the client went away mid-response; the status line is already
	// sent, so there is nothing useful left to do about it.
	_ = json.NewEncoder(w).Encode(body)
}

func writeStatus(w http.ResponseWriter, status int, reason string) {
	writeJSON(w, status, errorResponse{Error: reason})
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, statusFor(err), errorResponse{Error: err.Error()})
}
