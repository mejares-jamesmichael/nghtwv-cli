package audio

import (
	"errors"
	"sync"
)

// errRingClosed reports that the ring was closed while a writer was blocked.
var errRingClosed = errors.New("audio: ring closed")

// errOddWrite reports a write that would split a stereo frame.
var errOddWrite = errors.New("audio: write length must be a whole number of stereo frames")

// pcmRing is a bounded ring buffer of interleaved float32 samples. The decode
// loop writes; the output callback reads. Writes block while the ring is
// full, which bounds decode-ahead and turns a stalled output into
// backpressure instead of unbounded buffering.
//
// Reads never block (an empty ring returns 0, and the output callback answers
// with silence) and never split a stereo frame.
type pcmRing struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	buf      []float32
	r, w     int
	n        int
	closed   bool
}

// newPCMRing creates a ring with room for capacity samples; the capacity is
// rounded up to a whole number of stereo frames.
func newPCMRing(capacity int) *pcmRing {
	if capacity < 2 {
		capacity = 2
	}
	if capacity%2 != 0 {
		capacity++
	}
	ring := &pcmRing{buf: make([]float32, capacity)}
	ring.notEmpty = sync.NewCond(&ring.mu)
	ring.notFull = sync.NewCond(&ring.mu)
	return ring
}

// Write appends p, blocking while the ring is full. It returns the number of
// samples written and errRingClosed if the ring was closed mid-write. len(p)
// must be even so stereo frames are never split.
func (r *pcmRing) Write(p []float32) (int, error) {
	if len(p)%2 != 0 {
		return 0, errOddWrite
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	written := 0
	for len(p) > 0 {
		for r.n == len(r.buf) && !r.closed {
			r.notFull.Wait()
		}
		if r.closed {
			return written, errRingClosed
		}
		free := len(r.buf) - r.n
		chunk := len(p)
		if chunk > free {
			chunk = free
		}
		copied := copy(r.buf[r.w:], p[:chunk])
		if copied < chunk {
			copied += copy(r.buf, p[copied:chunk])
		}
		r.w = (r.w + copied) % len(r.buf)
		r.n += copied
		written += copied
		p = p[copied:]
		r.notEmpty.Signal()
	}
	return written, nil
}

// Read copies up to len(p) samples and never blocks; it returns 0 when the
// ring is empty. The count is always even.
func (r *pcmRing) Read(p []float32) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(p)
	if n > r.n {
		n = r.n
	}
	n &^= 1
	if n == 0 {
		return 0
	}
	first := copy(p, r.buf[r.r:min(r.r+n, len(r.buf))])
	if first < n {
		copy(p[first:n], r.buf[:n-first])
	}
	r.r = (r.r + n) % len(r.buf)
	r.n -= n
	r.notFull.Signal()
	return n
}

// Close wakes all blocked writers and readers. Subsequent writes fail with
// errRingClosed; reads drain whatever is left.
func (r *pcmRing) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.notFull.Broadcast()
	r.notEmpty.Broadcast()
}

// Reset drops all buffered samples and wakes blocked writers. It is used on
// resume-from-pause so stale audio is not replayed.
func (r *pcmRing) Reset() {
	r.mu.Lock()
	r.r, r.w, r.n = 0, 0, 0
	r.mu.Unlock()
	r.notFull.Broadcast()
}
