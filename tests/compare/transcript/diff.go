package transcript

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Difference is one place where the transcripts of the desktop and of
// grokbot-tunnel differ. A side without a value there is nil.
type Difference struct {
	// Path is a slash separated path into the compared value, e.g.
	// "requests/r1/2/client/shellStream/exit/code".
	Path    string
	Desktop *json.RawMessage
	Go      *json.RawMessage
}

func itoa(i int) string { return strconv.Itoa(i) }

// Diff returns the differences between two compared values (see
// Transcript.Compared), in path order.
func Diff(desktop, tunnel any) []Difference {
	var out []Difference
	diff(nil, desktop, tunnel, &out)
	return out
}

func raw(v any) *json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // compared values are JSON values
	}
	m := json.RawMessage(b)
	return &m
}

func diff(path []string, a, b any, out *[]Difference) {
	switch x := a.(type) {
	case map[string]any:
		if y, ok := b.(map[string]any); ok {
			diffObjects(path, x, y, out)
			return
		}
	case []any:
		if y, ok := b.([]any); ok {
			diffLists(path, x, y, out)
			return
		}
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, Difference{Path: strings.Join(path, "/"), Desktop: raw(a), Go: raw(b)})
	}
}

func diffObjects(path []string, a, b map[string]any, out *[]Difference) {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := append(path[:len(path):len(path)], k)
		av, aok := a[k]
		bv, bok := b[k]
		switch {
		case !aok || av == nil && bok && bv != nil:
			*out = append(*out, Difference{Path: strings.Join(p, "/"), Go: raw(bv)})
		case !bok || bv == nil && av != nil:
			*out = append(*out, Difference{Path: strings.Join(p, "/"), Desktop: raw(av)})
		default:
			diff(p, av, bv, out)
		}
	}
}

// diffLists skips the elements the lists share at their start and end, so
// that one extra frame gives one difference instead of shifting all that
// follow, and compares the rest by position.
func diffLists(path []string, a, b []any, out *[]Difference) {
	head := 0
	for head < len(a) && head < len(b) && reflect.DeepEqual(a[head], b[head]) {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && reflect.DeepEqual(a[len(a)-1-tail], b[len(b)-1-tail]) {
		tail++
	}
	am, bm := a[head:len(a)-tail], b[head:len(b)-tail]
	for i := 0; i < len(am) || i < len(bm); i++ {
		p := append(path[:len(path):len(path)], itoa(head+i))
		switch {
		case i >= len(am):
			*out = append(*out, Difference{Path: strings.Join(p, "/"), Go: raw(bm[i])})
		case i >= len(bm):
			*out = append(*out, Difference{Path: strings.Join(p, "/"), Desktop: raw(am[i])})
		default:
			diff(p, am[i], bm[i], out)
		}
	}
}
