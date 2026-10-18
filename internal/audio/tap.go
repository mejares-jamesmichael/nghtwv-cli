package audio

import "sync"

// Tap is a bounded, nonblocking copy point on the decoded PCM. The playback
// path writes and never blocks; the spectrum analyzer reads on its own
// schedule. On overflow the oldest samples are dropped, so a stalled
// analyzer can only make the spectrum skip ahead: it can never stall
// playback or grow memory.
type Tap struct {
	mu  sync.Mutex
	buf []float32
	r   int
	n   int
}

// newTap creates a tap with room for capacity interleaved samples.
func newTap(capacity int) *Tap {
	if capacity < 2 {
		capacity = 2
	}
	return &Tap{buf: make([]float32, capacity)}
}

// Write copies p into the tap, dropping the oldest samples if it does not
// fit. It never blocks.
func (t *Tap) Write(p []float32) {
	if len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= len(t.buf) {
		copy(t.buf, p[len(p)-len(t.buf):])
		t.r, t.n = 0, len(t.buf)
		return
	}
	if drop := t.n + len(p) - len(t.buf); drop > 0 {
		t.r = (t.r + drop) % len(t.buf)
		t.n -= drop
	}
	w := (t.r + t.n) % len(t.buf)
	first := copy(t.buf[w:], p)
	second := copy(t.buf, p[first:])
	t.n += first + second
}

// Read copies up to len(p) samples, oldest first, and never blocks.
func (t *Tap) Read(p []float32) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n > t.n {
		n = t.n
	}
	if n == 0 {
		return 0
	}
	first := copy(p, t.buf[t.r:min(t.r+n, len(t.buf))])
	if first < n {
		copy(p[first:n], t.buf[:n-first])
	}
	t.r = (t.r + n) % len(t.buf)
	t.n -= n
	return n
}

// cap reports the tap's capacity in samples.
func (t *Tap) cap() int { return len(t.buf) }
