package engine

import "testing"

// TestOrderStatusIsDerivedFromQuantities checks the four states the engine supports. Status
// is computed from the quantities rather than stored, so it cannot drift out of step with
// the book, and these are the only states an engine without pending, expired or replaced
// orders can produce.
func TestOrderStatusIsDerivedFromQuantities(t *testing.T) {
	cases := []struct {
		name                        string
		original, filled, remaining int64
		canceled                    int64
		want                        OrderStatus
		terminal                    bool
	}{
		{"untouched and resting", 100, 0, 100, 0, StatusOpen, false},
		{"partly executed and resting", 100, 40, 60, 0, StatusPartiallyFilled, false},
		{"fully executed", 100, 100, 0, 0, StatusFilled, true},
		{"withdrawn before trading", 100, 0, 0, 100, StatusCanceled, true},
		{"withdrawn after trading", 100, 40, 0, 60, StatusCanceled, true},
		{"market order remainder given up", 100, 70, 0, 30, StatusCanceled, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := &Order{
				OriginalQty:  tc.original,
				FilledQty:    tc.filled,
				RemainingQty: tc.remaining,
				CanceledQty:  tc.canceled,
			}
			if !o.Conserved() {
				t.Fatalf("the case itself does not conserve quantity: %+v", o)
			}
			if got := o.Status(); got != tc.want {
				t.Fatalf("status is %q, want %q", got, tc.want)
			}
			if got := o.Status().Terminal(); got != tc.terminal {
				t.Fatalf("terminal is %v, want %v", got, tc.terminal)
			}
			if got := o.View().Status; got != tc.want {
				t.Fatalf("the view reports status %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSubmitReportsRestingOrdersItFilled covers the engine half of passive lifecycle
// reporting: an execution changes two orders and both post-states must be available.
func TestSubmitReportsRestingOrdersItFilled(t *testing.T) {
	t.Run("partial fill of one resting order", func(t *testing.T) {
		h := newHarness(t)
		maker, _ := h.limit("maker", Sell, 10000, 100)

		_, res := h.market("taker", Buy, 40)

		if len(res.Filled) != 1 {
			t.Fatalf("reported %d filled resting orders, want 1", len(res.Filled))
		}
		passive := res.Filled[0]
		if passive.ID != maker {
			t.Fatalf("reported order %d, want the maker's %d", passive.ID, maker)
		}
		if passive.FilledQty != 40 || passive.RemainingQty != 60 {
			t.Fatalf("passive state is filled=%d leaves=%d, want 40 and 60", passive.FilledQty, passive.RemainingQty)
		}
		if passive.Status != StatusPartiallyFilled {
			t.Fatalf("passive status is %q, want %q", passive.Status, StatusPartiallyFilled)
		}
	})

	t.Run("sweep reports every level it consumed", func(t *testing.T) {
		h := newHarness(t)
		first, _ := h.limit("a", Sell, 10000, 10)
		second, _ := h.limit("b", Sell, 10010, 20)
		third, _ := h.limit("c", Sell, 10020, 30)

		_, res := h.market("taker", Buy, 45)

		if len(res.Filled) != 3 {
			t.Fatalf("reported %d filled resting orders, want 3", len(res.Filled))
		}
		// In the order they were hit, which is price-time priority order.
		wantIDs := []OrderID{first, second, third}
		wantFilled := []int64{10, 20, 15}
		wantStatus := []OrderStatus{StatusFilled, StatusFilled, StatusPartiallyFilled}
		for i := range wantIDs {
			got := res.Filled[i]
			if got.ID != wantIDs[i] {
				t.Fatalf("filled %d is order %d, want %d", i, got.ID, wantIDs[i])
			}
			if got.FilledQty != wantFilled[i] {
				t.Fatalf("filled %d reports %d executed, want %d", i, got.FilledQty, wantFilled[i])
			}
			if got.Status != wantStatus[i] {
				t.Fatalf("filled %d reports status %q, want %q", i, got.Status, wantStatus[i])
			}
		}
	})

	t.Run("a resting order is reported once per command", func(t *testing.T) {
		h := newHarness(t)
		h.limit("maker", Sell, 10000, 100)

		// The incoming order is the smaller side, so matching stops inside the level and
		// the resting order cannot be hit twice by one command.
		_, res := h.market("taker", Buy, 30)
		if len(res.Filled) != 1 {
			t.Fatalf("reported %d filled resting orders, want 1", len(res.Filled))
		}
	})

	t.Run("nothing to report when nothing traded", func(t *testing.T) {
		h := newHarness(t)
		_, res := h.limit("maker", Buy, 9990, 10)
		if len(res.Filled) != 0 {
			t.Fatalf("a resting order reported %d fills", len(res.Filled))
		}
	})

	t.Run("reported quantities always conserve", func(t *testing.T) {
		h := newHarness(t)
		for i := range 5 {
			h.limit("maker", Sell, PriceTicks(10000+i), 20)
		}
		_, res := h.market("taker", Buy, 75)

		var reported int64
		for _, v := range res.Filled {
			if v.FilledQty+v.RemainingQty+v.CanceledQty != v.OriginalQty {
				t.Fatalf("reported order does not conserve quantity: %+v", v)
			}
			reported += v.FilledQty
		}
		if reported != 75 {
			t.Fatalf("reported resting orders account for %d shares, want the 75 traded", reported)
		}
	})
}
