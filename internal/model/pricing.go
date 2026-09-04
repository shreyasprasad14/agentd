package model

import (
	"fmt"
	"math/big"
)

// MicroUSD is one millionth of a dollar. Costs are carried as integers so the
// runs.spent_usd counter never drifts from float rounding. NUMERIC(10,4) in
// Postgres holds the sum exactly after division by 1e6.
const MicroUSD = 1_000_000

// Price is the per-token price for one model, in micro-USD per million tokens
// (i.e. USD per million tokens, times 1e6). Anthropic-style pricing tables map
// directly: $3/MTok input becomes InputPerMTok = 3_000_000.
type Price struct {
	InputPerMTok  int64
	OutputPerMTok int64
}

// Cost prices usage against p. Integer arithmetic throughout; the division by
// one million tokens rounds toward zero, so a single call can under-count by
// at most one micro-USD.
func (p Price) Cost(u Usage) int64 {
	in := new(big.Int).Mul(big.NewInt(u.InputTokens), big.NewInt(p.InputPerMTok))
	out := new(big.Int).Mul(big.NewInt(u.OutputTokens), big.NewInt(p.OutputPerMTok))
	total := new(big.Int).Add(in, out)
	total.Div(total, big.NewInt(1_000_000))
	return total.Int64()
}

// FormatUSD renders micro-USD as a decimal string suitable for a numeric
// column or a log line, e.g. 1_234_500 -> "1.2345".
func FormatUSD(micro int64) string {
	sign := ""
	if micro < 0 {
		sign = "-"
		micro = -micro
	}
	return fmt.Sprintf("%s%d.%06d", sign, micro/MicroUSD, micro%MicroUSD)
}

// ParseUSD parses a decimal dollar string (as Postgres returns numeric) into
// micro-USD. Fractional digits beyond six are truncated.
func ParseUSD(s string) (int64, error) {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("parse usd %q", s)
	}
	r.Mul(r, big.NewRat(MicroUSD, 1))
	return new(big.Int).Quo(r.Num(), r.Denom()).Int64(), nil
}
