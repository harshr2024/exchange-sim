package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
	"github.com/harshraghuwanshi/exchange-sim/backend/internal/market"
)

func TestHealthAndMarketListing(t *testing.T) {
	r := newDefaultRig(t)

	raw := r.expectStatus(http.MethodGet, "/healthz", "", http.StatusOK)
	var health struct {
		Status      string   `json:"status"`
		Markets     []string `json:"markets"`
		Participant string   `json:"participant"`
	}
	decodeInto(t, raw, &health)
	if health.Status != "ok" {
		t.Fatalf("health reports %q", health.Status)
	}
	if health.Participant != DefaultParticipant {
		t.Fatalf("health reports participant %q, want %q", health.Participant, DefaultParticipant)
	}
	if len(health.Markets) != 2 {
		t.Fatalf("health lists %v, want the two configured markets", health.Markets)
	}

	r.expectStatus(http.MethodGet, "/api/markets", "", http.StatusOK)
}

func TestSubmitLimitOrderRestsOnTheBook(t *testing.T) {
	r := newDefaultRig(t)

	ack := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":50}`)

	if !ack.Accepted || !ack.Rested || ack.OrderID == 0 {
		t.Fatalf("ack is %+v, want an accepted resting order", ack)
	}
	if ack.Order.ParticipantID != DefaultParticipant {
		t.Fatalf("order is owned by %q, want %q", ack.Order.ParticipantID, DefaultParticipant)
	}
	if len(ack.Trades) != 0 {
		t.Fatalf("order traded against an empty book: %+v", ack.Trades)
	}

	snap := r.snapshot("NOVA")
	if !snap.HasBestBid || snap.BestBid != 9990 {
		t.Fatalf("best bid is (%d, %v), want (9990, true)", snap.BestBid, snap.HasBestBid)
	}
	if orders := r.openOrders("NOVA"); len(orders) != 1 || orders[0].ID != ack.OrderID {
		t.Fatalf("open orders are %+v, want the submitted order", orders)
	}
}

func TestSubmitMarketOrderFillsAndReportsTheRemainder(t *testing.T) {
	r := newDefaultRig(t)

	// Liquidity from another participant, which HTTP cannot create.
	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 30))

	ack := r.submit("NOVA", `{"side":"buy","type":"market","quantity":50}`)

	if len(ack.Trades) != 1 || ack.Trades[0].Quantity != 30 || ack.Trades[0].Price != 10010 {
		t.Fatalf("trades are %+v, want one fill of 30 at 10010", ack.Trades)
	}
	// A market order never rests; the unfillable part is canceled.
	if ack.Rested {
		t.Fatal("the market order rested on the book")
	}
	if ack.CanceledQty != 20 {
		t.Fatalf("ack reports %d canceled, want 20", ack.CanceledQty)
	}
	if orders := r.openOrders("NOVA"); len(orders) != 0 {
		t.Fatalf("the market order left %+v resting", orders)
	}
}

func TestSubmitSweepsMultipleLevels(t *testing.T) {
	r := newDefaultRig(t)

	for _, price := range []engine.PriceTicks{10010, 10020, 10030} {
		r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, price, 10))
	}

	// 30 shares are available across the three levels, so 40 sweeps all of them and
	// leaves 10 resting at the limit.
	ack := r.submit("NOVA", `{"side":"buy","type":"limit","price":10030,"quantity":40}`)

	if len(ack.Trades) != 3 {
		t.Fatalf("got %d trades, want a sweep of 3 levels: %+v", len(ack.Trades), ack.Trades)
	}
	wantPrices := []engine.PriceTicks{10010, 10020, 10030}
	for i, want := range wantPrices {
		if ack.Trades[i].Price != want {
			t.Fatalf("trade %d printed at %d, want %d", i, ack.Trades[i].Price, want)
		}
	}
	if !ack.Rested || ack.Order.RemainingQty != 10 {
		t.Fatalf("remainder is rested=%v with %d left, want true and 10", ack.Rested, ack.Order.RemainingQty)
	}
	if snap := r.snapshot("NOVA"); snap.HasBestAsk {
		t.Fatalf("the ask side survived the sweep: %+v", snap.Asks)
	}
}

// TestClientCannotChooseItsIdentity is the point of having no authentication but also no
// client-supplied participant: the browser is given an identity, and asking for a different
// one is an error rather than something quietly ignored.
func TestClientCannotChooseItsIdentity(t *testing.T) {
	r := newDefaultRig(t)

	raw := r.expectStatus(http.MethodPost, "/api/markets/NOVA/orders",
		`{"side":"buy","type":"limit","price":9990,"quantity":10,"participant":"bot-mm"}`,
		http.StatusBadRequest)
	if msg := r.errorMessage(raw); !strings.Contains(msg, "participant") {
		t.Fatalf("rejection reads %q, want it to name the offending field", msg)
	}

	// And a well-formed order is owned by the server's participant regardless.
	ack := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":10}`)
	if ack.Order.ParticipantID != DefaultParticipant {
		t.Fatalf("order is owned by %q", ack.Order.ParticipantID)
	}
}

// TestCancelEnforcesOwnership checks that the participant pinning actually protects other
// participants: a browser can cancel its own order and nothing else.
func TestCancelEnforcesOwnership(t *testing.T) {
	r := newDefaultRig(t)

	botOrder := r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Buy, 9980, 40))
	mine := r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":10}`)

	// Someone else's order: refused as forbidden, not silently ignored and not pretended
	// to be missing.
	raw := r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+itoa(uint64(botOrder.OrderID)), "", http.StatusForbidden)
	if msg := r.errorMessage(raw); !strings.Contains(msg, "another participant") {
		t.Fatalf("rejection reads %q, want it to mention ownership", msg)
	}

	// The bot's order is untouched.
	actor, _ := r.server.Exchange().Market("NOVA")
	botOrders, err := actor.OpenOrders(t.Context(), "bot-mm")
	if err != nil {
		t.Fatalf("reading the bot's orders: %v", err)
	}
	if len(botOrders) != 1 {
		t.Fatalf("the bot has %d resting orders, want 1", len(botOrders))
	}

	// My own order cancels cleanly.
	r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+itoa(uint64(mine.OrderID)), "", http.StatusOK)
	if orders := r.openOrders("NOVA"); len(orders) != 0 {
		t.Fatalf("my order is still resting: %+v", orders)
	}

	// Cancelling it again is a miss, and the engine reports one consistent answer for an
	// order that is gone whatever the reason.
	r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+itoa(uint64(mine.OrderID)), "", http.StatusNotFound)
}

func TestCancelUnknownOrderIsNotFound(t *testing.T) {
	r := newDefaultRig(t)
	r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/4242", "", http.StatusNotFound)
}

func TestUnknownSymbolIsNotFound(t *testing.T) {
	r := newDefaultRig(t)

	for _, path := range []string{
		"/api/markets/TSLA/snapshot",
		"/api/markets/TSLA/trades",
		"/api/markets/TSLA/orders",
	} {
		r.expectStatus(http.MethodGet, path, "", http.StatusNotFound)
	}
	r.expectStatus(http.MethodPost, "/api/markets/TSLA/orders",
		`{"side":"buy","type":"limit","price":100,"quantity":1}`, http.StatusNotFound)
	r.expectStatus(http.MethodDelete, "/api/markets/TSLA/orders/1", "", http.StatusNotFound)

	// PULSE is listed as an instrument but was not opened on this exchange, which is a
	// different kind of miss and must also be a 404 rather than a panic.
	r.expectStatus(http.MethodGet, "/api/markets/PULSE/snapshot", "", http.StatusNotFound)
}

func TestSymbolsAreCaseInsensitive(t *testing.T) {
	r := newDefaultRig(t)

	r.submit("nova", `{"side":"buy","type":"limit","price":9990,"quantity":10}`)
	if snap := r.snapshot("NOVA"); !snap.HasBestBid {
		t.Fatal("an order routed via a lower-case symbol did not reach the market")
	}
}

func TestMalformedOrderRequests(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"empty body", ``, http.StatusBadRequest},
		{"not json", `not json`, http.StatusBadRequest},
		{"unknown side", `{"side":"sideways","type":"limit","price":100,"quantity":1}`, http.StatusBadRequest},
		{"missing side", `{"type":"limit","price":100,"quantity":1}`, http.StatusBadRequest},
		{"unknown type", `{"side":"buy","type":"stop","price":100,"quantity":1}`, http.StatusBadRequest},
		{"missing type", `{"side":"buy","price":100,"quantity":1}`, http.StatusBadRequest},
		{"zero quantity", `{"side":"buy","type":"limit","price":100,"quantity":0}`, http.StatusBadRequest},
		{"negative quantity", `{"side":"buy","type":"limit","price":100,"quantity":-5}`, http.StatusBadRequest},
		{"zero price", `{"side":"buy","type":"limit","quantity":10}`, http.StatusBadRequest},
		{"negative price", `{"side":"buy","type":"limit","price":-100,"quantity":10}`, http.StatusBadRequest},
		{"priced market order", `{"side":"buy","type":"market","price":100,"quantity":10}`, http.StatusBadRequest},
		{"unknown field", `{"side":"buy","type":"limit","price":100,"quantity":10,"tif":"ioc"}`, http.StatusBadRequest},
		{"client supplied order id", `{"side":"buy","type":"limit","price":100,"quantity":10,"order_id":7}`, http.StatusBadRequest},
	}

	r := newDefaultRig(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := r.expectStatus(http.MethodPost, "/api/markets/NOVA/orders", tc.body, tc.want)
			if msg := r.errorMessage(raw); msg == "" {
				t.Fatal("rejection carries no reason")
			}
		})
	}

	// None of that junk may have reached the book.
	if orders := r.openOrders("NOVA"); len(orders) != 0 {
		t.Fatalf("malformed requests created %+v", orders)
	}
	if snap := r.snapshot("NOVA"); snap.HasBestBid || snap.HasBestAsk {
		t.Fatalf("malformed requests changed the book: %+v", snap)
	}
}

func TestMalformedCancelPath(t *testing.T) {
	r := newDefaultRig(t)

	for _, id := range []string{"abc", "0", "-1", "1.5"} {
		r.expectStatus(http.MethodDelete, "/api/markets/NOVA/orders/"+id, "", http.StatusBadRequest)
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	r := newDefaultRig(t)

	body := `{"side":"buy","type":"limit","price":100,"quantity":1,"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	r.expectStatus(http.MethodPost, "/api/markets/NOVA/orders", body, http.StatusBadRequest)
}

func TestTradesEndpoint(t *testing.T) {
	r := newDefaultRig(t)

	r.actorSubmit("NOVA", market.LimitOrder("bot-mm", engine.Sell, 10010, 100))
	r.submit("NOVA", `{"side":"buy","type":"market","quantity":10}`)
	r.submit("NOVA", `{"side":"buy","type":"market","quantity":20}`)

	raw := r.expectStatus(http.MethodGet, "/api/markets/NOVA/trades", "", http.StatusOK)
	var trades []engine.Trade
	decodeInto(t, raw, &trades)
	if len(trades) != 2 {
		t.Fatalf("got %d trades, want 2", len(trades))
	}
	if trades[0].Quantity != 10 || trades[1].Quantity != 20 {
		t.Fatalf("trades are out of order or wrong: %+v", trades)
	}

	// Limit keeps the most recent.
	raw = r.expectStatus(http.MethodGet, "/api/markets/NOVA/trades?limit=1", "", http.StatusOK)
	decodeInto(t, raw, &trades)
	if len(trades) != 1 || trades[0].Quantity != 20 {
		t.Fatalf("limit=1 returned %+v, want the most recent trade", trades)
	}

	r.expectStatus(http.MethodGet, "/api/markets/NOVA/trades?limit=nope", "", http.StatusBadRequest)
	r.expectStatus(http.MethodGet, "/api/markets/NOVA/trades?limit=-1", "", http.StatusBadRequest)

	// An untraded market returns an empty array rather than null, so clients need no
	// special case.
	raw = r.expectStatus(http.MethodGet, "/api/markets/ORBIT/trades", "", http.StatusOK)
	if got := strings.TrimSpace(string(raw)); got != "[]" {
		t.Fatalf("an untraded market returned %q, want []", got)
	}
}

func TestMarketsAreIsolated(t *testing.T) {
	r := newDefaultRig(t)

	r.submit("NOVA", `{"side":"buy","type":"limit","price":9990,"quantity":10}`)

	if snap := r.snapshot("ORBIT"); snap.HasBestBid || snap.Sequence != 0 {
		t.Fatalf("an order on NOVA reached ORBIT: %+v", snap)
	}
	if orders := r.openOrders("ORBIT"); len(orders) != 0 {
		t.Fatalf("ORBIT holds %+v", orders)
	}
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
