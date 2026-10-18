package plaza

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Endpoint parity with plaza-player.
const (
	StatusURL      = "https://api.plaza.one/status"
	HistoryURL     = "https://api.plaza.one/history"
	BackgroundsURL = "https://api.plaza.one/v2/backgrounds/random"

	// ResponseLimit mirrors MAX_STATUS_BYTES (256 KiB + 1 read check).
	ResponseLimit = 256 * 1024

	// HistoryLimit mirrors HISTORY_LIMIT.
	HistoryLimit = 10

	// fetchTimeout mirrors urlopen(timeout=3).
	fetchTimeout = 3 * time.Second
)

// Client fetches the Plaza API. Zero-value URL fields fall back to the
// parity endpoints, so tests only override what they exercise.
type Client struct {
	StatusURL      string
	HistoryURL     string
	BackgroundsURL string

	// UserAgent is sent with every request (locked decision: nghtwv/<version>).
	UserAgent string

	MaxBytes int64

	http *http.Client
}

// New builds the default client for the given ldflags-injected version.
func New(userAgent string) *Client {
	return &Client{
		StatusURL:      StatusURL,
		HistoryURL:     HistoryURL,
		BackgroundsURL: BackgroundsURL,
		UserAgent:      userAgent,
		MaxBytes:       ResponseLimit,
		http:           &http.Client{Timeout: fetchTimeout},
	}
}

// fetchJSON performs the parity fetch: UA + Accept headers, 3 s budget,
// 256 KiB accumulation cap, JSON decode. Returns the raw payload.
func (c *Client) fetchJSON(ctx context.Context, url string) ([]byte, error) {
	if c.http == nil {
		c.http = &http.Client{Timeout: fetchTimeout}
	}
	limit := c.MaxBytes
	if limit <= 0 {
		limit = ResponseLimit
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Parity message shape for failed responses (urllib's HTTPError str).
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("Plaza API response is too large")
	}
	return raw, nil
}
