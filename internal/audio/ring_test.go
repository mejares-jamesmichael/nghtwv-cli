package audio

import (
	"errors"
	"testing"
	"time"
)

func TestPCMRingRoundTrip(t *testing.T) {
	ring := newPCMRing(8)
	if n, err := ring.Write([]float32{1, 2, 3, 4, 5, 6}); err != nil || n != 6 {
		t.Fatalf("Write = %d, %v; want 6, nil", n, err)
	}
	out := make([]float32, 4)
	if n := ring.Read(out); n != 4 || out[0] != 1 || out[3] != 4 {
		t.Fatalf("Read = %d %v; want 1..4", n, out)
	}
	if n := ring.Read(out[:2]); n != 2 || out[0] != 5 || out[1] != 6 {
		t.Fatalf("Read = %d %v; want 5,6", n, out[:2])
	}
	if n := ring.Read(out); n != 0 {
		t.Fatalf("Read on empty = %d; want 0", n)
	}
}

func TestPCMRingWraps(t *testing.T) {
	ring := newPCMRing(8)
	if _, err := ring.Write([]float32{1, 2, 3, 4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	out := make([]float32, 6)
	ring.Read(out)
	// The read and write cursors are now both at offset 6; the next write
	// wraps both.
	if _, err := ring.Write([]float32{7, 8, 9, 10, 11, 12}); err != nil {
		t.Fatal(err)
	}
	if n := ring.Read(out); n != 6 || out[0] != 7 || out[5] != 12 {
		t.Fatalf("after wrap Read = %d %v; want 7..12", n, out)
	}
}

func TestPCMRingWriteBlocksUntilRead(t *testing.T) {
	ring := newPCMRing(4)
	done := make(chan error, 1)
	go func() {
		_, err := ring.Write([]float32{1, 2, 3, 4, 5, 6})
		done <- err
	}()

	out := make([]float32, 6)
	total := 0
	deadline := time.After(2 * time.Second)
	for total < 6 {
		total += ring.Read(out[total:])
		if total == 6 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("writer never completed; drained %d of 6", total)
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Write did not return after the ring was drained")
	}
}

func TestPCMRingCloseUnblocksWriter(t *testing.T) {
	ring := newPCMRing(4)
	if _, err := ring.Write([]float32{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ring.Write([]float32{5, 6})
		done <- err
	}()
	time.Sleep(10 * time.Millisecond) // let the writer park on the full ring
	ring.Close()
	select {
	case err := <-done:
		if !errors.Is(err, errRingClosed) {
			t.Fatalf("Write after Close = %v; want errRingClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not unblock the writer")
	}
}

func TestPCMRingRejectsPartialFrames(t *testing.T) {
	ring := newPCMRing(8)
	if _, err := ring.Write([]float32{1, 2, 3}); !errors.Is(err, errOddWrite) {
		t.Fatalf("odd Write error = %v; want errOddWrite", err)
	}
}

func TestPCMRingReadStaysFrameAligned(t *testing.T) {
	ring := newPCMRing(8)
	if _, err := ring.Write([]float32{1, 2, 3, 4, 5, 6}); err != nil {
		t.Fatal(err)
	}
	out := make([]float32, 5) // odd request
	if n := ring.Read(out); n != 4 {
		t.Fatalf("odd-sized Read = %d; want 4", n)
	}
}
