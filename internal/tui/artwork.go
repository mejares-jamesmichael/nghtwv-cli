package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	_ "image/jpeg" // plaza artwork is jpg
	_ "image/png"
)

// Rendered artwork cell box (half-block cells keep square-ish pixels).
const (
	artworkCols    = 24
	artworkRows    = 12
	artBoxWidth    = artworkCols + 2 // rounded border adds one cell each side
	artPanelHeight = artworkRows + 2
	halfUpGlyph    = "▀" // top pixel = foreground, bottom pixel = background
)

const imageUserAgent = "nghtwv" // CDN fetch identifier; API calls carry nghtwv/<version>

// artCellCache memoizes styled half-up blocks per color pair; the View
// runs on bubbletea's single render goroutine, so no lock is needed.
var artCellCache = map[[6]uint32]lipgloss.Style{}

// httpImages is the default ImageSource (the tui-declared seam; tests
// swap in a fake).
type httpImages struct {
	client   *http.Client
	maxBytes int64
}

// newImageSource builds the default artwork pipeline (6 s budget, 512 KiB
// cap).
func newImageSource() *httpImages {
	return &httpImages{
		client:   &http.Client{Timeout: 6 * time.Second},
		maxBytes: 512 * 1024,
	}
}

// Fetch downloads and decodes the artwork image.
func (h *httpImages) Fetch(ctx context.Context, url string) (image.Image, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", imageUserAgent)
	req.Header.Set("Accept", "image/png, image/jpeg, */*")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, h.maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > h.maxBytes {
		return nil, errors.New("artwork response is too large")
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	return img, err
}

// renderHalfBlocks paints an image as truecolor half-block cells,
// nearest-neighbor sampled: cell (col, row) renders "▀" with the top-half
// pixel as foreground and the bottom-half pixel as background.
func renderHalfBlocks(img image.Image, cols, rows int) string {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 || cols <= 0 || rows <= 0 {
		return ""
	}

	var b strings.Builder
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			px := bounds.Min.X + (2*col+1)*w/(2*cols)
			tr, tg, tb := rgbAt(img, px, bounds.Min.Y+(4*row+1)*h/(4*rows))
			br, bg, bb := rgbAt(img, px, bounds.Min.Y+(4*row+3)*h/(4*rows))

			key := [6]uint32{tr, tg, tb, br, bg, bb}
			style, ok := artCellCache[key]
			if !ok {
				style = lipgloss.NewStyle().
					Foreground(lipgloss.Color(rgbHex([3]uint32{tr, tg, tb}))).
					Background(lipgloss.Color(rgbHex([3]uint32{br, bg, bb})))
				artCellCache[key] = style
			}
			b.WriteString(style.Render(halfUpGlyph))
		}
		if row+1 < rows {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// rgbAt samples one pixel, clamped to the image rect.
func rgbAt(img image.Image, x, y int) (uint32, uint32, uint32) {
	bounds := img.Bounds()
	x = max(bounds.Min.X, min(bounds.Max.X-1, x))
	y = max(bounds.Min.Y, min(bounds.Max.Y-1, y))
	r0, g0, b0, _ := img.At(x, y).RGBA()
	return r0 >> 8, g0 >> 8, b0 >> 8
}

func rgbHex(rgb [3]uint32) string {
	return fmt.Sprintf("#%02x%02x%02x", rgb[0], rgb[1], rgb[2])
}
