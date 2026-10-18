package plaza

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureClient serves one testdata file for every endpoint the client
// supports.
func fixtureClient(t *testing.T, fixture string) *Client {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	c := New("nghtwv-test")
	c.StatusURL = srv.URL
	c.HistoryURL = srv.URL
	c.BackgroundsURL = srv.URL
	return c
}

// checkGolden marshals got (compact single line + newline) and compares it
// with testdata/golden/<name>. GOLDEN_UPDATE=1 rewrites goldens.
func checkGolden(t *testing.T, got any, name string) {
	t.Helper()
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := append(b, '\n')

	goldenPath := filepath.Join("testdata", "golden", name)
	if os.Getenv("GOLDEN_UPDATE") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(goldenPath, out, 0o644); err != nil {
			t.Fatalf("update golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden %s missing: %v (run with GOLDEN_UPDATE=1 to write it)", name, err)
	}
	if string(want) != string(out) {
		t.Fatalf("golden %s mismatch:\n got: %s\nwant: %s", name, out, want)
	}
}

func TestMetadataLive(t *testing.T) {
	c := fixtureClient(t, "status_live.json")
	m, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	checkGolden(t, m, "metadata_live.json")
}

func TestMetadataFallbackWalk(t *testing.T) {
	c := fixtureClient(t, "status_fallback.json")
	m, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	checkGolden(t, m, "metadata_fallback.json")
}

func TestMetadataEdgeTolerances(t *testing.T) {
	c := fixtureClient(t, "status_edge.json")
	m, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	checkGolden(t, m, "metadata_edge.json")
}

func TestMetadataNullSong(t *testing.T) {
	c := fixtureClient(t, "status_nosong.json")
	m, err := c.Metadata(context.Background())
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	checkGolden(t, m, "metadata_nosong.json")
}

func TestHistoryLive(t *testing.T) {
	c := fixtureClient(t, "history_live.json")
	h, err := c.History(context.Background())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	checkGolden(t, h, "history_live.json")
}

func TestHistoryEmptyShape(t *testing.T) {
	// Unknown payloads still emit {"songs": []} (parity print).
	c := fixtureClient(t, "status_nosong.json")
	h, err := c.History(context.Background())
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if h == nil || h.Songs == nil || len(h.Songs) != 0 {
		t.Fatalf("songs must be an empty array, got %+v", h)
	}
}

func TestBackgroundsLive(t *testing.T) {
	c := fixtureClient(t, "backgrounds_live.json")
	b, err := c.Backgrounds(context.Background())
	if err != nil {
		t.Fatalf("Backgrounds: %v", err)
	}
	checkGolden(t, b, "backgrounds_live.json")
}

func TestBackgroundsNoData(t *testing.T) {
	c := fixtureClient(t, "backgrounds_nodata.json")
	_, err := c.Backgrounds(context.Background())
	if err == nil || err.Error() != "Plaza backgrounds response has no data" {
		t.Fatalf("err = %v, want parity no-data message", err)
	}
}

func TestBackgroundsNoImage(t *testing.T) {
	c := fixtureClient(t, "backgrounds_nosrc.json")
	_, err := c.Backgrounds(context.Background())
	if err == nil || err.Error() != "Plaza backgrounds response has no image" {
		t.Fatalf("err = %v, want parity no-image message", err)
	}
}

func TestResponseTooLargeParityMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("x", int(ResponseLimit)+100))) // >256KiB
	}))
	t.Cleanup(srv.Close)
	c := New("nghtwv-test")
	c.StatusURL = srv.URL
	if _, err := c.Metadata(context.Background()); err == nil ||
		err.Error() != "Plaza API response is too large" {
		t.Fatalf("err = %v, want the parity size-cap message", err)
	}
}

func TestHTTPErrorParityMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c := New("nghtwv-test")
	c.StatusURL = srv.URL
	_, err := c.Metadata(context.Background())
	if err == nil || err.Error() != "HTTP Error 500: Internal Server Error" {
		t.Fatalf("err = %v, want urllib-shaped HTTP error text", err)
	}
}

// TestFindValueOrdering locks the walker's fidelity to the plugin:
// breadth-first scalar scan over an object's keys before descending.
func TestFindValueOrdering(t *testing.T) {
	tree, err := parseTree([]byte(`{"a":{"title":"inner"},"Title":"outer"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := tree.findValue([]string{"title"})
	if found == nil || found.str != "outer" {
		t.Fatalf("breadth scan must prefer the outer key (json order aside), got %+v", found)
	}
}

func TestFindValueCaseFoldingAndArrays(t *testing.T) {
	tree, err := parseTree([]byte(`{"NOW_PLAYING":["skip",{"Name":{"source_link":1},"name":"deep"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Parity: key matches only return SCALAR children; containers are
	// descended into, never returned themselves.
	if found := tree.findValue([]string{"now_playing"}); found != nil {
		t.Fatalf("container children must not match directly, got %+v", found)
	}
	deep := tree.findValue([]string{"name"})
	if deep == nil || deep.str != "deep" {
		t.Fatalf("array recursion failed, got %+v", deep)
	}
}

// TestUserAgentHeader pins the locked nghtwv/<version> identification.
func TestUserAgentHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); ua != "nghtwv/9.9.test" {
			t.Errorf("User-Agent = %q, want nghtwv/9.9.test", ua)
		}
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("Accept = %q", accept)
		}
		_, _ = w.Write([]byte(`{"song":null}`))
	}))
	t.Cleanup(srv.Close)
	c := New("nghtwv/9.9.test")
	c.StatusURL = srv.URL
	if _, err := c.Metadata(context.Background()); err != nil {
		t.Fatalf("Metadata: %v", err)
	}
}

// TestLiveAPI is the optional live harness (NGHTWV_IT=1): real endpoints,
// real shapes; asserts decode/pipeline sanity only, no audio involved.
func TestLiveAPI(t *testing.T) {
	if os.Getenv("NGHTWV_IT") != "1" {
		t.Skip("set NGHTWV_IT=1 to hit the live Plaza API")
	}
	c := New("nghtwv-integration-test")
	ctx := context.Background()
	m, err := c.Metadata(ctx)
	if err != nil {
		t.Skipf("live status unreachable (network/environment): %v", err)
	}
	_ = m
	h, err := c.History(ctx)
	if err != nil {
		t.Skipf("live history unreachable: %v", err)
	}
	_ = h
	b, err := c.Backgrounds(ctx)
	if err != nil {
		t.Skipf("live backgrounds unreachable: %v", err)
	}
	_ = b
}
