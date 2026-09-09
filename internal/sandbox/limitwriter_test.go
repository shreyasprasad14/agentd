package sandbox

import (
	"bytes"
	"testing"
)

func TestLimitWriter(t *testing.T) {
	w := newLimitWriter(5)
	for _, chunk := range []string{"ab", "cd"} {
		if n, err := w.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if w.Truncated() {
		t.Fatal("truncated before the budget was reached")
	}
	if n, _ := w.Write([]byte("efg")); n != 3 {
		t.Fatalf("Write past the budget must report all bytes consumed, got %d", n)
	}
	if got := w.Bytes(); !bytes.Equal(got, []byte("abcde")) {
		t.Fatalf("Bytes = %q, want the first five", got)
	}
	if !w.Truncated() || w.Total() != 7 {
		t.Fatalf("Truncated = %v, Total = %d", w.Truncated(), w.Total())
	}

	exact := newLimitWriter(3)
	_, _ = exact.Write([]byte("xyz"))
	if exact.Truncated() {
		t.Fatal("writing exactly the budget is not truncation")
	}
	_, _ = exact.Write(nil)
	if exact.Truncated() {
		t.Fatal("an empty write is not truncation")
	}
}

func TestLimitsDefaults(t *testing.T) {
	l := Limits{}.withDefaults()
	if l != DefaultLimits() {
		t.Fatalf("zero Limits should resolve to the defaults, got %+v", l)
	}
	if got := l.Timeout(0); got != l.DefaultTimeout {
		t.Fatalf("Timeout(0) = %s", got)
	}
	if got := l.Timeout(l.MaxTimeout * 10); got != l.MaxTimeout {
		t.Fatalf("Timeout above max = %s", got)
	}
	short := Limits{DefaultTimeout: 10 * l.MaxTimeout, MaxTimeout: l.MaxTimeout}.withDefaults()
	if short.DefaultTimeout != l.MaxTimeout {
		t.Fatalf("default above max should be clamped, got %s", short.DefaultTimeout)
	}
}

func TestValidateFileName(t *testing.T) {
	for _, ok := range []string{"main.py", "data.json", "a b.txt", ".hidden"} {
		if err := ValidateFileName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/b", `a\b`, "/etc/passwd", "nul\x00"} {
		if err := ValidateFileName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
