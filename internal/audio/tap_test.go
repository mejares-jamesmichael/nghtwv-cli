package audio

import "testing"

func TestTapKeepsNewestOnOverflow(t *testing.T) {
	tap := newTap(8)
	tap.Write([]float32{1, 2, 3, 4, 5, 6})
	tap.Write([]float32{7, 8, 9, 10})

	out := make([]float32, 8)
	if n := tap.Read(out); n != 8 {
		t.Fatalf("Read = %d; want 8", n)
	}
	want := []float32{3, 4, 5, 6, 7, 8, 9, 10}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("Read = %v; want %v", out, want)
		}
	}
}

func TestTapWriteLargerThanCapacity(t *testing.T) {
	tap := newTap(4)
	tap.Write([]float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10})

	out := make([]float32, 4)
	if n := tap.Read(out); n != 4 {
		t.Fatalf("Read = %d; want 4", n)
	}
	want := []float32{7, 8, 9, 10}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("Read = %v; want %v", out, want)
		}
	}
}

func TestTapReadWraps(t *testing.T) {
	tap := newTap(4)
	tap.Write([]float32{1, 2})
	drained := make([]float32, 2)
	if n := tap.Read(drained); n != 2 || drained[0] != 1 || drained[1] != 2 {
		t.Fatalf("first Read = %v (n=%d); want [1 2]", drained, n)
	}
	tap.Write([]float32{3, 4, 5, 6})

	out := make([]float32, 4)
	if n := tap.Read(out); n != 4 {
		t.Fatalf("Read = %d; want 4", n)
	}
	want := []float32{3, 4, 5, 6}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("Read = %v; want %v", out, want)
		}
	}
}

func TestTapReadIsNonBlockingWhenEmpty(t *testing.T) {
	tap := newTap(4)
	if n := tap.Read(make([]float32, 4)); n != 0 {
		t.Fatalf("Read on empty tap = %d; want 0", n)
	}
}
