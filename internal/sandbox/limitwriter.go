package sandbox

import "sync"

// limitWriter keeps the first max bytes written to it and counts the rest.
// It never returns an error, so a producer that floods stdout is drained to
// completion rather than being stalled on a full pipe (which would turn a
// noisy script into a timeout).
type limitWriter struct {
	mu    sync.Mutex
	max   int
	buf   []byte
	total int64
}

func newLimitWriter(max int) *limitWriter {
	return &limitWriter{max: max}
}

func (w *limitWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += int64(n)
	if room := w.max - len(w.buf); room > 0 {
		if n > room {
			p = p[:room]
		}
		w.buf = append(w.buf, p...)
	}
	return n, nil
}

// Bytes returns the kept prefix.
func (w *limitWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buf...)
}

// Truncated reports whether anything was dropped.
func (w *limitWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total > int64(len(w.buf))
}

// Total is the number of bytes the process actually wrote.
func (w *limitWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}
