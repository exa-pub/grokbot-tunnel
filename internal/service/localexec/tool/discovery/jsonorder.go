package discovery

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
)

// member is a property of a JSON object.
type member struct {
	key   string
	value json.RawMessage
}

// objectMembers returns the properties of a JSON object in the order
// JavaScript enumerates them after JSON.parse: array indices ascending, then
// other keys in the order they first appear; a repeated key keeps its first
// place and its last value. ok is false if raw is not an object.
func objectMembers(raw []byte) ([]member, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	var members []member
	index := map[string]int{}
	for dec.More() {
		tok, err := dec.Token()
		key, isKey := tok.(string)
		var value json.RawMessage
		if err != nil || !isKey || dec.Decode(&value) != nil {
			return nil, false
		}
		if i, seen := index[key]; seen {
			members[i].value = value
			continue
		}
		index[key] = len(members)
		members = append(members, member{key, value})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := dec.Token(); err == nil {
		return nil, false // trailing data
	}
	slices.SortStableFunc(members, func(a, b member) int {
		ai, aok := arrayIndex(a.key)
		bi, bok := arrayIndex(b.key)
		switch {
		case aok && bok:
			return int(ai - bi)
		case aok:
			return -1
		case bok:
			return 1
		}
		return 0
	})
	return members, true
}

// arrayIndex reports a key that JavaScript treats as an array index.
func arrayIndex(key string) (int64, bool) {
	n, err := strconv.ParseUint(key, 10, 32)
	if err != nil || n == 1<<32-1 || strconv.FormatUint(n, 10) != key {
		return 0, false
	}
	return int64(n), true
}

// field returns the value of a property of a JSON object, or nil.
func field(members []member, key string) json.RawMessage {
	for _, m := range members {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

// stringField returns a property that is a JSON string, as a zod
// string().optional().catch(undefined) reads it.
func stringField(members []member, key string) (string, bool) {
	var s string
	raw := bytes.TrimSpace(field(members, key))
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

// numberField returns a property that is a JSON number. present is set when
// the property exists, so that a value of another type can be refused.
func numberField(members []member, key string) (n float64, present, ok bool) {
	raw := field(members, key)
	if raw == nil {
		return 0, false, false
	}
	return n, true, json.Unmarshal(raw, &n) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
