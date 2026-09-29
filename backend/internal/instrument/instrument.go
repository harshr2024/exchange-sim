// Package instrument describes the fictional securities listed on the exchange.
//
// The matching engine is generic with respect to symbol; nothing in this package
// is consulted on the matching path. It exists so the API and UI can label
// prices and so the simulated traders have a reference price to bootstrap from.
package instrument

import (
	"fmt"
	"sort"
	"strings"

	"github.com/harshraghuwanshi/exchange-sim/backend/internal/engine"
)

// Instrument is a listed fictional security.
type Instrument struct {
	Symbol      string `json:"symbol"`
	DisplayName string `json:"display_name"`
	// TickSizeCents is the value of one price tick, in cents. A tick size of 1
	// means prices are quoted in whole cents and 10010 ticks reads as $100.10.
	TickSizeCents int64 `json:"tick_size_cents"`
	// InitialReferencePrice bootstraps the liquidity bots. It is never treated
	// as a traded price: the displayed last price only moves when a trade prints.
	InitialReferencePrice engine.PriceTicks `json:"initial_reference_price"`
}

var listed = map[string]Instrument{
	"NOVA":  {Symbol: "NOVA", DisplayName: "Nova Dynamics", TickSizeCents: 1, InitialReferencePrice: 10000},
	"ORBIT": {Symbol: "ORBIT", DisplayName: "Orbit Logistics", TickSizeCents: 1, InitialReferencePrice: 4250},
	"PULSE": {Symbol: "PULSE", DisplayName: "Pulse Biotech", TickSizeCents: 1, InitialReferencePrice: 18775},
}

// ErrUnknownSymbol is returned for a symbol that is not listed.
var ErrUnknownSymbol = fmt.Errorf("unknown symbol")

// Lookup returns the instrument for symbol. Symbols are case-insensitive on the
// way in and always canonical (upper case) on the way out.
func Lookup(symbol string) (Instrument, error) {
	inst, ok := listed[strings.ToUpper(symbol)]
	if !ok {
		return Instrument{}, fmt.Errorf("%w: %q", ErrUnknownSymbol, symbol)
	}
	return inst, nil
}

// All returns every listed instrument, ordered by symbol.
func All() []Instrument {
	out := make([]Instrument, 0, len(listed))
	for _, inst := range listed {
		out = append(out, inst)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Symbol < out[j].Symbol })
	return out
}

// Symbols returns every listed symbol, ordered.
func Symbols() []string {
	out := make([]string, 0, len(listed))
	for sym := range listed {
		out = append(out, sym)
	}
	sort.Strings(out)
	return out
}

// Format renders a tick price for display, e.g. 10010 ticks -> "100.10".
func (i Instrument) Format(p engine.PriceTicks) string {
	cents := int64(p) * i.TickSizeCents
	neg := ""
	if cents < 0 {
		neg, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", neg, cents/100, cents%100)
}
