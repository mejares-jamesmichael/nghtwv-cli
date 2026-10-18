package audio

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gopxl/beep/v2"
)

var format44100 = beep.Format{SampleRate: 44100, NumChannels: 2, Precision: 4}

// fakeOutput records everything written to it. pace simulates a draining
// device so pause/stop timing is observable; ops records the write/flush
// ordering, which is what pause and stop correctness depend on. A non-nil
// block models a writer parked on a full ring: Flush releases it, like the
// real ring's reset does.
type fakeOutput struct {
	mu       sync.Mutex
	frames   []float32
	ops      []string
	blocked  int
	block    chan struct{}
	pace     time.Duration
	writeErr error
}

func (f *fakeOutput) Devices() ([]Device, error) { return nil, nil }
func (f *fakeOutput) Default() (Device, error)   { return Device{}, nil }
func (f *fakeOutput) Open(string) error          { return nil }
func (f *fakeOutput) Underflow() bool            { return false }
func (f *fakeOutput) Close() error               { return nil }

func (f *fakeOutput) Write(p []float32) error {
	if f.pace > 0 {
		time.Sleep(f.pace)
	}
	f.mu.Lock()
	block := f.block
	if block != nil {
		f.blocked++
	}
	f.mu.Unlock()
	if block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.frames = append(f.frames, p...)
	op := "write:audio"
	if isSilence(p) {
		op = "write:silence"
	}
	f.ops = append(f.ops, op)
	return nil
}

func (f *fakeOutput) Flush() error {
	f.mu.Lock()
	f.releaseBlockedLocked()
	f.ops = append(f.ops, "flush")
	f.mu.Unlock()
	return nil
}

// releaseBlocked frees a writer parked on the fake's gate.
func (f *fakeOutput) releaseBlocked() {
	f.mu.Lock()
	f.releaseBlockedLocked()
	f.mu.Unlock()
}

func (f *fakeOutput) releaseBlockedLocked() {
	if f.block != nil {
		close(f.block)
		f.block = nil
	}
}

func (f *fakeOutput) blockedWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blocked
}

func (f *fakeOutput) timeline() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.ops))
	copy(out, f.ops)
	return out
}

func (f *fakeOutput) clearOps() {
	f.mu.Lock()
	f.ops = nil
	f.mu.Unlock()
}

func isSilence(p []float32) bool {
	for _, v := range p {
		if v != 0 {
			return false
		}
	}
	return true
}

func (f *fakeOutput) written() []float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float32, len(f.frames))
	copy(out, f.frames)
	return out
}

// constStream produces a fixed sample value and reports drained once remain
// is exhausted.
type constStream struct {
	remain int
	value  float64
}

func (s *constStream) Stream(p [][2]float64) (int, bool) {
	if s.remain <= 0 {
		return 0, false
	}
	n := len(p)
	if n > s.remain {
		n = s.remain
	}
	for i := range p[:n] {
		p[i][0] = s.value
		p[i][1] = s.value
	}
	s.remain -= n
	return n, s.remain > 0
}

func (s *constStream) Err() error   { return nil }
func (s *constStream) Close() error { return nil }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestEngineVolumeCurve(t *testing.T) {
	// mpv's mixer (audio_get_gain in player/audio.c) computes
	// pow(volume/100, 3); these are the gains a persisted player volume
	// produced through mpv and must still produce here.
	for _, tc := range []struct {
		volume int
		gain   float64
	}{
		{0, 0}, {20, 0.008}, {50, 0.125}, {80, 0.512}, {100, 1},
	} {
		t.Run(fmt.Sprintf("volume %d", tc.volume), func(t *testing.T) {
			out := &fakeOutput{}
			opener := func(ctx context.Context) (Stream, beep.Format, error) {
				return &constStream{remain: 4096, value: 1}, format44100, nil
			}
			engine := NewEngine(out, 44100, opener,
				WithVolume(tc.volume), WithChunkFrames(256), WithBackoff(time.Millisecond, 4*time.Millisecond))
			engine.Play()
			defer engine.Stop()

			waitFor(t, "audio", func() bool { return len(out.written()) >= 512 })
			if got := float64(out.written()[0]); math.Abs(got-tc.gain) > 1e-6 {
				t.Fatalf("gain = %v; want %v (volume %d)", got, tc.gain, tc.volume)
			}
			if volume := engine.Status().Volume; volume != tc.volume {
				t.Fatalf("volume = %d; want %d", volume, tc.volume)
			}
		})
	}
}

func TestEnginePauseDropsQueuedAudioAndResumeFlushes(t *testing.T) {
	out := &fakeOutput{pace: time.Millisecond}
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		return &constStream{remain: 1 << 30, value: 1}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener, WithChunkFrames(64))
	engine.Play()
	defer engine.Stop()

	waitFor(t, "first audio", func() bool {
		got := out.written()
		return len(got) >= 128 && got[0] == 1
	})

	// Pause must flush queued audio before the silence: without it, up to
	// the ring's half second would keep playing after Pause.
	out.clearOps()
	engine.Pause()
	waitFor(t, "paused state", func() bool { return engine.Status().State == StatePaused })
	waitFor(t, "pause flush", func() bool {
		for _, op := range out.timeline() {
			if op == "flush" {
				return true
			}
		}
		return false
	})
	waitFor(t, "silence after pause", func() bool {
		for _, op := range out.timeline() {
			if op == "write:silence" {
				return true
			}
		}
		return false
	})
	ops := out.timeline()
	flushed := false
	for i, op := range ops {
		if op == "flush" {
			flushed = true
			continue
		}
		if flushed && op != "write:silence" {
			t.Fatalf("op %d after pause flush = %q (%v); want only silence", i, op, ops)
		}
	}

	time.Sleep(20 * time.Millisecond)
	mark := len(out.written())
	time.Sleep(30 * time.Millisecond)
	tail := out.written()[mark:]
	if len(tail) == 0 {
		t.Fatal("no frames written while paused")
	}
	for i, v := range tail {
		if v != 0 {
			t.Fatalf("paused output has %v at index %d; want silence", v, i)
		}
	}

	// Resume must flush the buffered silence before the first real audio.
	out.clearOps()
	engine.Play()
	waitFor(t, "playing state", func() bool { return engine.Status().State == StatePlaying })
	waitFor(t, "audio after resume", func() bool {
		got := out.written()
		return len(got) > 0 && got[len(got)-1] == 1
	})
	ops = out.timeline()
	flushAt, audioAt := -1, -1
	for i, op := range ops {
		if op == "flush" && flushAt < 0 {
			flushAt = i
		}
		if op == "write:audio" && audioAt < 0 {
			audioAt = i
		}
	}
	if flushAt < 0 {
		t.Fatalf("no flush after resume: %v", ops)
	}
	if audioAt < 0 || flushAt > audioAt {
		t.Fatalf("resume ops = %v; want a flush before the first audio write", ops)
	}
}

func TestEngineReconnectsAfterOpenerErrors(t *testing.T) {
	out := &fakeOutput{}
	var mu sync.Mutex
	attempts := 0
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts <= 2 {
			return nil, beep.Format{}, errors.New("connect failed")
		}
		return &constStream{remain: 2048, value: 0.5}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener,
		WithChunkFrames(256), WithBackoff(time.Millisecond, 4*time.Millisecond))
	engine.Play()
	defer engine.Stop()

	waitFor(t, "playback", func() bool { return len(out.written()) >= 512 })
	waitFor(t, "playing state", func() bool { return engine.Status().State == StatePlaying })

	mu.Lock()
	got := attempts
	mu.Unlock()
	if got < 3 {
		t.Fatalf("opener attempts = %d; want >= 3", got)
	}
	if err := engine.Err(); err != nil {
		t.Fatalf("Err() = %v after successful reconnect; want nil", err)
	}
}

func TestEngineStopHaltsWriting(t *testing.T) {
	out := &fakeOutput{pace: time.Millisecond}
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		return &constStream{remain: 1 << 30, value: 1}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener, WithChunkFrames(64))
	engine.Play()
	waitFor(t, "audio", func() bool { return len(out.written()) > 64 })

	engine.Stop()
	if state := engine.Status().State; state != StateStopped {
		t.Fatalf("state after Stop = %v; want stopped", state)
	}
	time.Sleep(30 * time.Millisecond) // let any in-flight write land
	mark := len(out.written())
	time.Sleep(30 * time.Millisecond)
	if got := len(out.written()); got != mark {
		t.Fatalf("wrote %d more frames after Stop", got-mark)
	}
}

func TestEngineStopsRetryingWhenDeviceFails(t *testing.T) {
	out := &fakeOutput{writeErr: ErrClosed}
	var mu sync.Mutex
	attempts := 0
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return &constStream{remain: 1 << 20, value: 1}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener,
		WithChunkFrames(64), WithBackoff(time.Millisecond, 4*time.Millisecond))
	engine.Play()
	defer engine.Stop()

	waitFor(t, "stopped state", func() bool { return engine.Status().State == StateStopped })
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("opener attempts = %d; want 1 (no retry when the device is gone)", got)
	}
	if err := engine.Err(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Err() = %v; want ErrClosed", err)
	}
}

func TestEngineStopFlushesQueuedAudio(t *testing.T) {
	out := &fakeOutput{pace: time.Millisecond}
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		return &constStream{remain: 1 << 30, value: 1}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener, WithChunkFrames(64))
	engine.Play()
	waitFor(t, "audio", func() bool { return len(out.written()) > 128 })

	engine.Stop()
	if state := engine.Status().State; state != StateStopped {
		t.Fatalf("state = %v; want stopped", state)
	}
	// Stop waits for the decode loop, so the last operation must be a flush:
	// no samples may be written after it, and the ring must be empty.
	ops := out.timeline()
	if len(ops) == 0 || ops[len(ops)-1] != "flush" {
		t.Fatalf("ops after Stop = %v; want a flush last", ops)
	}
	mark := len(out.timeline())
	time.Sleep(30 * time.Millisecond)
	if got := out.timeline(); len(got) != mark {
		t.Fatalf("pump wrote %v after Stop returned", got[mark:])
	}
}

func TestEngineStopWhileWriteIsBlocked(t *testing.T) {
	out := &fakeOutput{block: make(chan struct{})}
	defer out.releaseBlocked()
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		return &constStream{remain: 1 << 30, value: 1}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener, WithChunkFrames(64))
	engine.Play()
	defer engine.Stop()
	waitFor(t, "blocked write", func() bool { return out.blockedWrites() > 0 })

	// Flush must wake the parked writer, otherwise Stop would hang.
	stopped := make(chan struct{})
	go func() {
		engine.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop hung while the decode loop was blocked in Write")
	}
}

func TestEngineReportsBufferingWhenStreamEnds(t *testing.T) {
	out := &fakeOutput{pace: time.Millisecond}
	var mu sync.Mutex
	attempts := 0
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		return &constStream{remain: 6400, value: 1}, format44100, nil
	}
	// The backoff is far longer than the test, so a buffering state can only
	// come from the drained stream, never from a failed retry.
	engine := NewEngine(out, 44100, opener,
		WithChunkFrames(64), WithBackoff(2*time.Second, 2*time.Second))
	engine.Play()
	defer engine.Stop()

	waitFor(t, "playing", func() bool { return engine.Status().State == StatePlaying })
	waitFor(t, "buffering after the stream drained", func() bool {
		return engine.Status().State == StateBuffering
	})

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	got := attempts
	mu.Unlock()
	if got != 1 {
		t.Fatalf("opener attempts = %d; want 1 during the backoff", got)
	}
	if state := engine.Status().State; state != StateBuffering {
		t.Fatalf("state = %v; want buffering during the outage", state)
	}
}

func TestEngineVolumeClamps(t *testing.T) {
	engine := NewEngine(&fakeOutput{}, 44100, func(ctx context.Context) (Stream, beep.Format, error) {
		return nil, beep.Format{}, errors.New("not opened")
	})
	if got := engine.Status().Volume; got != 100 {
		t.Fatalf("default volume = %d; want 100", got)
	}
	engine.SetVolume(140)
	if got := engine.Volume(); got != 100 {
		t.Fatalf("volume after 140 = %d; want 100", got)
	}
	engine.SetVolume(-5)
	if got := engine.Volume(); got != 0 {
		t.Fatalf("volume after -5 = %d; want 0", got)
	}
	if state := engine.Status().State; state != StateStopped {
		t.Fatalf("state = %v; want stopped", state)
	}
}
