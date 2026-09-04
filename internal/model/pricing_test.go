package model

import "testing"

func TestPriceCost(t *testing.T) {
	cases := []struct {
		name  string
		price Price
		usage Usage
		want  int64
	}{
		{"zero price is free", Price{}, Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, 0},
		{"exact million tokens", Price{InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000}, Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000}, 18_000_000},
		{"small call", Price{InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000}, Usage{InputTokens: 1000, OutputTokens: 200}, 3000 + 3000},
		{"rounds toward zero", Price{InputPerMTok: 1, OutputPerMTok: 0}, Usage{InputTokens: 999_999}, 0},
		{"no overflow at large counts", Price{InputPerMTok: 15_000_000, OutputPerMTok: 75_000_000}, Usage{InputTokens: 10_000_000_000, OutputTokens: 1_000_000_000}, 150_000_000_000 + 75_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.price.Cost(tc.usage); got != tc.want {
				t.Fatalf("Cost = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestFormatParseUSD(t *testing.T) {
	cases := []struct {
		micro int64
		str   string
	}{
		{0, "0.000000"},
		{1, "0.000001"},
		{1_234_500, "1.234500"},
		{-500_000, "-0.500000"},
		{100 * MicroUSD, "100.000000"},
	}
	for _, tc := range cases {
		if got := FormatUSD(tc.micro); got != tc.str {
			t.Errorf("FormatUSD(%d) = %q, want %q", tc.micro, got, tc.str)
		}
		back, err := ParseUSD(tc.str)
		if err != nil {
			t.Fatalf("ParseUSD(%q): %v", tc.str, err)
		}
		if back != tc.micro {
			t.Errorf("ParseUSD(%q) = %d, want %d", tc.str, back, tc.micro)
		}
	}

	// Postgres renders NUMERIC(10,4) with four places.
	got, err := ParseUSD("1.0000")
	if err != nil || got != MicroUSD {
		t.Fatalf("ParseUSD(1.0000) = %d, %v", got, err)
	}
	if _, err := ParseUSD("abc"); err == nil {
		t.Fatal("expected parse error")
	}
}
