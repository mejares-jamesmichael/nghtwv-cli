package plaza

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// node is an ordered JSON tree: object member order is preserved so that
// findValue reproduces plaza-player's dict-iteration-order-dependent
// first-match semantics exactly.
type node struct {
	kind nodeKind

	str string  // scalarString
	num float64 // scalarNumber
	b   bool    // scalarBool

	object []member // objectKind, in document order
	array  []*node  // arrayKind, in document order
}

type nodeKind int

const (
	scalarNull nodeKind = iota
	scalarBool
	scalarNumber
	scalarString
	objectKind
	arrayKind
)

type member struct {
	key string
	val *node
}

func (n *node) scalar() bool {
	switch n.kind {
	case scalarBool, scalarNumber, scalarString:
		return true
	}
	return false
}

// parseTree decodes raw JSON into the ordered tree.
func parseTree(raw []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	first, err := dec.Token()
	if err != nil {
		return nil, err
	}
	tree, err := parseValue(dec, first)
	if err != nil {
		return nil, err
	}
	// Reject trailing garbage like a bare json.Unmarshal would.
	var extra json.Token
	if err := dec.Decode(&extra); err == nil {
		return nil, errTrailing
	}
	return tree, nil
}

// errTrailing surfaces non-whitespace trailing data, matching a bare
// json.Unmarshal's strictness.
var errTrailing = &json.UnmarshalTypeError{Value: "trailing data"}

// findValue is the faithful port of plaza-player's find_value over an
// ordered tree:
//
//  1. scan every direct key of an object IN ORDER for a scalar match;
//  2. only then descend into the object's container children in order;
//  3. arrays are walked in order, keyless.
//
// Keys match case-insensitively; scalars include bools because Python's
// isinstance(True, int) is true (consumers filter).
func (n *node) findValue(names []string) *node {
	switch n.kind {
	case objectKind:
		for _, m := range n.object {
			if !m.val.scalar() {
				continue
			}
			for _, name := range names {
				if strings.EqualFold(m.key, name) {
					return m.val
				}
			}
		}
		for _, m := range n.object {
			if m.val.kind == objectKind || m.val.kind == arrayKind {
				if found := m.val.findValue(names); found != nil {
					return found
				}
			}
		}
	case arrayKind:
		for _, child := range n.array {
			if found := child.findValue(names); found != nil {
				return found
			}
		}
	}
	return nil
}

// get returns the direct child with the most-so-far-first matching key
// (exact, not folded — parity with dict.get).
func (n *node) get(key string) *node {
	if n == nil || n.kind != objectKind {
		return nil
	}
	for _, m := range n.object {
		if m.key == key {
			return m.val
		}
	}
	return nil
}

func parseValue(dec *json.Decoder, tok json.Token) (*node, error) {
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			obj := &node{kind: objectKind}
			for {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if d, ok := keyTok.(json.Delim); ok && d == '}' {
					return obj, nil
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, &json.UnmarshalTypeError{Value: "non-string key"}
				}
				valTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				child, err := parseValue(dec, valTok)
				if err != nil {
					return nil, err
				}
				obj.object = append(obj.object, member{key: key, val: child})
			}
		}
		if t == '[' {
			arr := &node{kind: arrayKind}
			for {
				valTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if d, ok := valTok.(json.Delim); ok && d == ']' {
					return arr, nil
				}
				child, err := parseValue(dec, valTok)
				if err != nil {
					return nil, err
				}
				arr.array = append(arr.array, child)
			}
		}
		return nil, &json.UnmarshalTypeError{Value: "unexpected delimiter"}
	case string:
		return &node{kind: scalarString, str: t}, nil
	case float64:
		return &node{kind: scalarNumber, num: t}, nil
	case bool:
		return &node{kind: scalarBool, b: t}, nil
	case nil:
		return &node{kind: scalarNull}, nil
	default:
		return nil, &json.UnmarshalTypeError{Value: fmt.Sprint(t)}
	}
}
