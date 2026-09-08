package anthropic

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/shreyasprasad/agentd/internal/model"
)

// usd builds a Price from USD-per-million-token rates. Cache reads and writes
// default to Anthropic's standard 10% and 125% of the input rate; models with
// a different cache rate pass it explicitly.
func usd(in, out float64, cache ...float64) model.Price {
	p := model.Price{
		InputPerMTok:      int64(in * model.MicroUSD),
		OutputPerMTok:     int64(out * model.MicroUSD),
		CacheReadPerMTok:  int64(in * 0.10 * model.MicroUSD),
		CacheWritePerMTok: int64(in * 1.25 * model.MicroUSD),
	}
	if len(cache) > 0 {
		p.CacheReadPerMTok = int64(cache[0] * model.MicroUSD)
	}
	if len(cache) > 1 {
		p.CacheWritePerMTok = int64(cache[1] * model.MicroUSD)
	}
	return p
}

// DefaultPrices is the first-party Anthropic API price list as of 2026-09-08,
// USD per million tokens. Keys match by longest prefix, so a dated snapshot
// id such as "claude-opus-4-5-20251101" prices as "claude-opus-4-5". A model
// with no entry cannot be called: Complete refuses rather than record a $0
// cost that would let a run sail past its budget. Add entries with
// Config.Price.
var DefaultPrices = map[string]model.Price{
	"claude-fable-5-1":  usd(10, 50, 0.25),
	"claude-fable-5":    usd(10, 50),
	"claude-mythos-5-1": usd(10, 50, 0.25),
	"claude-opus-5":     usd(5, 25),
	"claude-opus-4-8":   usd(5, 25),
	"claude-opus-4-7":   usd(5, 25),
	"claude-opus-4-6":   usd(5, 25),
	"claude-opus-4-5":   usd(5, 25),
	"claude-sonnet-5":   usd(2, 10),
	"claude-sonnet-4-6": usd(3, 15),
	"claude-sonnet-4-5": usd(3, 15),
	"claude-haiku-4-5":  usd(1, 5),
}

// priceTable resolves a model id to a Price by longest matching key.
type priceTable struct {
	prices map[string]model.Price
	keys   []string // longest first
}

func newPriceTable(overrides map[string]model.Price) priceTable {
	t := priceTable{prices: make(map[string]model.Price, len(DefaultPrices)+len(overrides))}
	for k, v := range DefaultPrices {
		t.prices[k] = v
	}
	for k, v := range overrides {
		t.prices[k] = v
	}
	for k := range t.prices {
		t.keys = append(t.keys, k)
	}
	sort.Slice(t.keys, func(i, j int) bool {
		if len(t.keys[i]) != len(t.keys[j]) {
			return len(t.keys[i]) > len(t.keys[j])
		}
		return t.keys[i] < t.keys[j]
	})
	return t
}

func (t priceTable) lookup(modelID string) (model.Price, bool) {
	if p, ok := t.prices[modelID]; ok {
		return p, true
	}
	for _, k := range t.keys {
		if strings.HasPrefix(modelID, k) {
			return t.prices[k], true
		}
	}
	return model.Price{}, false
}

// ParsePrices parses an operator-supplied price list of the form
// "model=input/output[,model=input/output...]" in USD per million tokens,
// e.g. "claude-opus-6=6/30,claude-haiku-5=1.5/7.5". Cache rates follow the
// standard 10% / 125% rule.
func ParsePrices(s string) (map[string]model.Price, error) {
	out := map[string]model.Price{}
	for _, entry := range strings.Split(s, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		name, rates, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("price %q: want model=input/output", entry)
		}
		inStr, outStr, ok := strings.Cut(rates, "/")
		if !ok {
			return nil, fmt.Errorf("price %q: want model=input/output", entry)
		}
		in, err := strconv.ParseFloat(strings.TrimSpace(inStr), 64)
		if err != nil {
			return nil, fmt.Errorf("price %q: input rate: %w", entry, err)
		}
		o, err := strconv.ParseFloat(strings.TrimSpace(outStr), 64)
		if err != nil {
			return nil, fmt.Errorf("price %q: output rate: %w", entry, err)
		}
		if in < 0 || o < 0 {
			return nil, fmt.Errorf("price %q: rates must not be negative", entry)
		}
		out[strings.TrimSpace(name)] = usd(in, o)
	}
	return out, nil
}
