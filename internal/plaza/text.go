package plaza

import (
	"math"
	"strconv"
	"strings"
	"unicode"
)

// cleanText mirrors plaza-player's clean_text: collapse unicode whitespace
// runs to single spaces (Python's bare str.split), join, then cut at
// `limit` runes (Python slices characters, not bytes).
func cleanText(value string, limit int) string {
	out := strings.Join(strings.FieldsFunc(value, unicode.IsSpace), " ")
	return cutRunes(out, limit)
}

// cutRunes slices to at most `limit` runes (Python str[:limit]).
func cutRunes(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return s
}

func isSpaceRune(r rune) bool {
	return unicode.IsSpace(r)
}

// toInt reproduces Python's int(float(value)) wrapped in try/except —
// numbers, bools (isinstance(True, int) is true), and numeric strings
// convert; everything else (null, non-numeric strings, containers)
// reports failure so callers fall back to their defaults. Python's float()
// raises on inf/NaN at the int() step, so non-finite strings fail too.
func toInt(n *node) (int, bool) {
	switch n.kind {
	case scalarNumber:
		return int(truncToZero(n.num)), true
	case scalarBool:
		if n.b {
			return 1, true
		}
		return 0, true
	case scalarString:
		f, err := strconv.ParseFloat(strings.TrimSpace(n.str), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return 0, false
		}
		return int(truncToZero(f)), true
	default:
		return 0, false
	}
}

// toIntNumberOnly is the stricter variant for sites the plugin gated with
// isinstance(value, (int, float)) before converting (listeners).
func toIntNumberOnly(n *node) (int, bool) {
	switch n.kind {
	case scalarNumber:
		return int(truncToZero(n.num)), true
	case scalarBool:
		if n.b {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// toIntPython reproduces Python's bare int(value) wrapped in try/except —
// used for the background id: numbers truncate toward zero, bools convert,
// and strings must be pure decimal integers (int("12") works, but
// int("12.5") raises ValueError, unlike the float-paths above).
func toIntPython(n *node) (int, bool) {
	switch n.kind {
	case scalarNumber:
		return int(truncToZero(n.num)), true
	case scalarBool:
		if n.b {
			return 1, true
		}
		return 0, true
	case scalarString:
		i, err := strconv.Atoi(strings.TrimSpace(n.str))
		if err != nil {
			return 0, false
		}
		return i, true
	default:
		return 0, false
	}
}

// truncToZero matches Python int(float) truncation toward zero.
func truncToZero(f float64) float64 {
	return math.Trunc(f)
}
