package audio

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopxl/beep/v2"
)

var format44100 = beep.Format{SampleRate: 44100, NumChannels: 2, Precision: 4}

// fakeOutput records everything written to it. pace simulates a draining
// device so pause/stop timing is observable.
type fakeOutput struct {
	mu       sync.Mutex
	frames   []float32
	flushes  int
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
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.frames = append(f.frames, p...)
	return nil
}

func (f *fakeOutput) Flush() error {
	f.mu.Lock()
	f.flushes++
	f.mu.Unlock()
	return nil
}

func (f *fakeOutput) written() []float32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]float32, len(f.frames))
	copy(out, f.frames)
	return out
}

func (f *fakeOutput) flushCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
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

func TestEnginePumpsWithVolumeGain(t *testing.T) {
	out := &fakeOutput{}
	opener := func(ctx context.Context) (Stream, beep.Format, error) {
		return &constStream{remain: 4096, value: 1}, format44100, nil
	}
	engine := NewEngine(out, 44100, opener,
		WithVolume(50), WithChunkFrames(256), WithBackoff(time.Millisecond, 4*time.Millisecond))
	engine.Play()
	defer engine.Stop()

	waitFor(t, "audio", func() bool { return len(out.written()) >= 512 })
	got := out.written()
	// Volume 50 on the cubic curve is a gain of 0.125.
	if got[0] != 0.125 {
		t.Fatalf("first sample = %v; want 0.125 (volume 50)", got[0])
	}
	waitFor(t, "playing state", func() bool { return engine.Status().State == StatePlaying })
	if volume := engine.Status().Volume; volume != 50 {
		t.Fatalf("volume = %d; want 50", volume)
	}
}

func TestEnginePauseWritesSilenceAndResumeFlushes(t *testing.T) {
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

	engine.Pause()
	waitFor(t, "paused state", func() bool { return engine.Status().State == StatePaused })
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

	engine.Play() // resume
	waitFor(t, "playing state", func() bool { return engine.Status().State == StatePlaying })
	waitFor(t, "flush on resume", func() bool { return out.flushCount() > 0 })
	waitFor(t, "audio after resume", func() bool {
		got := out.written()
		return len(got) > 0 && got[len(got)-1] == 1
	})
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
