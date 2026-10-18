package plaza

import (
	"context"
	"fmt"
)

// Metadata is the CLI JSON shape of the metadata command; pointer ints
// preserve the plugin's include-only-when-valid key emission ("position"
// is present even at 0 as long as length is known).
type Metadata struct {
	Title     string `json:"title,omitempty"`
	Artist    string `json:"artist,omitempty"`
	Track     string `json:"track,omitempty"`
	Album     string `json:"album,omitempty"`
	Artwork   string `json:"artwork,omitempty"`
	Length    *int   `json:"length,omitempty"`
	Position  *int   `json:"position,omitempty"`
	Listeners *int   `json:"listeners,omitempty"`
}

// HistoryEntry is one past track.
type HistoryEntry struct {
	Artist   string `json:"artist,omitempty"`
	Track    string `json:"track,omitempty"`
	PlayedAt *int   `json:"played_at,omitempty"`
}

// History is the CLI JSON shape of the history command ("songs" always
// present, possibly empty).
type History struct {
	Songs []HistoryEntry `json:"songs"`
}

// Background is the CLI JSON shape of the backgrounds command.
type Background struct {
	ID     *int   `json:"id,omitempty"`
	Src    string `json:"src,omitempty"`
	Author string `json:"author,omitempty"`
	Source string `json:"source,omitempty"`
}

var titleSearchKeys = []string{"nowplaying", "now_playing", "track", "song", "title"}

var listenerSearchKeys = []string{"listeners", "listener_count", "listeners_count", "current_listeners"}

// Metadata fetches api/status and builds the parity result.
func (c *Client) Metadata(ctx context.Context) (*Metadata, error) {
	raw, err := c.fetchJSON(ctx, c.StatusURL)
	if err != nil {
		return nil, err
	}
	tree, err := parseTree(raw)
	if err != nil {
		return nil, err
	}
	return metadataFrom(tree), nil
}

func metadataFrom(data *node) *Metadata {
	var artist, track, album, artwork string
	length, position := 0, 0

	if song := data.get("song"); song != nil && song.kind == objectKind {
		if v := song.get("artist"); v != nil && v.kind == scalarString {
			artist = cleanText(v.str, 160)
		}
		if v := song.get("title"); v != nil && v.kind == scalarString {
			track = cleanText(v.str, 160)
		}
		if v := song.get("album"); v != nil && v.kind == scalarString {
			album = cleanText(v.str, 160)
		}
		// artwork_sm_src or artwork_src — python's `a or b` falsy fallback,
		// cleaned at whichever candidate is picked.
		if sm := song.get("artwork_sm_src"); sm != nil && sm.kind == scalarString && sm.str != "" {
			artwork = cleanText(sm.str, 512)
		} else if src := song.get("artwork_src"); src != nil && src.kind == scalarString && src.str != "" {
			artwork = cleanText(src.str, 512)
		}
		if raw := song.get("length"); raw != nil {
			if v, ok := toInt(raw); ok && v > 0 {
				length = v
			}
		}
		if raw := song.get("position"); raw != nil {
			if v, ok := toInt(raw); ok && v > 0 {
				position = v
			}
		}
	}

	// title: "artist - track" / artist / track, else a recursive search for
	// any plausible title string (dict-hop included), else omitted.
	title := ""
	hasTitle := false
	switch {
	case artist != "" && track != "":
		title = cleanTextJoin(artist, track)
		hasTitle = true
	case artist != "":
		title, hasTitle = artist, true
	case track != "":
		title, hasTitle = track, true
	default:
		if found := data.findValue(titleSearchKeys); found != nil {
			if found.kind == objectKind {
				found = found.findValue([]string{"title", "name"})
			}
			if found != nil && found.kind == scalarString {
				title = cleanText(found.str, 160)
				hasTitle = true
			}
		}
	}

	m := &Metadata{}
	// python: isinstance(title, str) and title.strip() → emit title[:160].
	if hasTitle && stringsHaveContent(title) {
		m.Title = cutRunes(title, 160)
	}
	if artist != "" {
		m.Artist = artist
	}
	if track != "" {
		m.Track = track
	}
	if album != "" {
		m.Album = album
	}
	if artwork != "" {
		m.Artwork = artwork
	}
	if length > 0 {
		l := length
		m.Length = &l
		pos := position
		if pos > length {
			pos = length
		}
		m.Position = &pos
	}
	if found := data.findValue(listenerSearchKeys); found != nil {
		if v, ok := toIntNumberOnly(found); ok && v >= 0 {
			m.Listeners = &v
		}
	}
	return m
}

// cleanTextJoin renders "artist - track".
func cleanTextJoin(artist, track string) string {
	return artist + " - " + track
}

func stringsHaveContent(s string) bool {
	for _, r := range s {
		if !isSpaceRune(r) {
			return true
		}
	}
	return false
}

// History fetches api/history; songs are sliced to the limit FIRST
// (parity: invalid entries inside the first ten still consume slots).
func (c *Client) History(ctx context.Context) (*History, error) {
	raw, err := c.fetchJSON(ctx, c.HistoryURL)
	if err != nil {
		return nil, err
	}
	tree, err := parseTree(raw)
	if err != nil {
		return nil, err
	}
	return historyFrom(tree), nil
}

func historyFrom(data *node) *History {
	h := &History{Songs: []HistoryEntry{}} // always emitted, possibly empty
	songs := data.get("songs")
	if songs == nil || songs.kind != arrayKind {
		return h
	}
	for i, song := range songs.array {
		if i >= HistoryLimit {
			break
		}
		if song == nil || song.kind != objectKind {
			continue
		}
		var artist, track string
		if v := song.get("artist"); v != nil && v.kind == scalarString {
			artist = cleanText(v.str, 160)
		}
		if v := song.get("title"); v != nil && v.kind == scalarString {
			track = cleanText(v.str, 160)
		}
		if artist == "" && track == "" {
			continue
		}
		entry := HistoryEntry{}
		if artist != "" {
			entry.Artist = artist
		}
		if track != "" {
			entry.Track = track
		}
		if raw := song.get("played_at"); raw != nil {
			if pa, ok := toInt(raw); ok && pa > 0 {
				played := pa
				entry.PlayedAt = &played
			}
		}
		h.Songs = append(h.Songs, entry)
	}
	return h
}

// Backgrounds fetches v2/backgrounds/random (the API randomizes
// server-side; every call is a fresh shuffle).
func (c *Client) Backgrounds(ctx context.Context) (*Background, error) {
	raw, err := c.fetchJSON(ctx, c.BackgroundsURL)
	if err != nil {
		return nil, err
	}
	tree, err := parseTree(raw)
	if err != nil {
		return nil, err
	}
	return backgroundFrom(tree)
}

func backgroundFrom(data *node) (*Background, error) {
	item := data.get("data")
	if item == nil || item.kind != objectKind {
		return nil, fmt.Errorf("Plaza backgrounds response has no data")
	}
	b := &Background{}
	if raw := item.get("id"); raw != nil {
		if id, ok := toIntPython(raw); ok && id > 0 {
			value := id
			b.ID = &value
		}
	}
	if raw := item.get("src"); raw != nil && raw.kind == scalarString {
		b.Src = cleanText(raw.str, 512)
	}
	if raw := item.get("author"); raw != nil && raw.kind == scalarString {
		b.Author = cleanText(raw.str, 160)
	}
	if raw := item.get("source"); raw != nil && raw.kind == scalarString {
		b.Source = cleanText(raw.str, 160)
	}
	if b.Src == "" {
		return nil, fmt.Errorf("Plaza backgrounds response has no image")
	}
	return b, nil
}
