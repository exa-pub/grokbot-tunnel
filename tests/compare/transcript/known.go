package transcript

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
)

// Known is a difference from the desktop that is there on purpose, or is
// known and not fixed yet.
type Known struct {
	// Scenario is a scenario ID or a pattern of path.Match ("shell/*").
	Scenario string `json:"scenario"`
	// Path is a pattern over Difference.Path, as in Rule.Path.
	Path string `json:"path"`
	// Desktop and Go, when given, are the values the difference must have.
	Desktop *json.RawMessage `json:"desktop,omitempty"`
	Go      *json.RawMessage `json:"go,omitempty"`
	// Reason says why the difference stays.
	Reason string `json:"reason"`
	// Ref points to where it is discussed.
	Ref string `json:"ref,omitempty"`
}

// Registry is the list of known differences and which of them were seen.
type Registry struct {
	known []Known
	used  []bool
	ran   map[string]bool
}

// LoadKnown reads a known-diffs.json; a missing file is an empty list.
func LoadKnown(file string) (*Registry, error) {
	r := &Registry{ran: map[string]bool{}}
	b, err := os.ReadFile(file) //nolint:gosec // a path given by the test
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&r.known); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	for i, k := range r.known {
		if k.Scenario == "" || k.Path == "" || k.Reason == "" {
			return nil, fmt.Errorf("%s: entry %d needs scenario, path and reason", file, i)
		}
		if _, err := path.Match(k.Scenario, ""); err != nil {
			return nil, fmt.Errorf("%s: entry %d: %w", file, i, err)
		}
	}
	r.used = make([]bool, len(r.known))
	return r, nil
}

func (k Known) covers(scenario string, d Difference) bool {
	if ok, err := path.Match(k.Scenario, scenario); err != nil || !ok {
		return false
	}
	if !matchPath(strings.Split(k.Path, "/"), strings.Split(d.Path, "/")) {
		return false
	}
	return sameJSON(k.Desktop, d.Desktop) && sameJSON(k.Go, d.Go)
}

// sameJSON reports whether got is the value want asks for; a nil want asks
// for nothing.
func sameJSON(want, got *json.RawMessage) bool {
	if want == nil {
		return true
	}
	if got == nil {
		return false
	}
	var a, b any
	return json.Unmarshal(*want, &a) == nil && json.Unmarshal(*got, &b) == nil && fmt.Sprint(a) == fmt.Sprint(b)
}

// Split divides the differences of a scenario into those the registry knows
// and those it does not, and remembers which entries it used.
func (r *Registry) Split(scenario string, diffs []Difference) (known, unexpected []Difference) {
	r.ran[scenario] = true
	for _, d := range diffs {
		covered := false
		for i, k := range r.known {
			if k.covers(scenario, d) {
				r.used[i], covered = true, true
			}
		}
		if covered {
			known = append(known, d)
		} else {
			unexpected = append(unexpected, d)
		}
	}
	return known, unexpected
}

// Stale returns the entries that no difference matched although every
// scenario they name ran: the difference is gone and its entry must go too.
// all is the list of scenarios that exist.
func (r *Registry) Stale(all []string) []Known {
	var out []Known
	for i, k := range r.known {
		if r.used[i] {
			continue
		}
		matched, complete := false, true
		for _, id := range all {
			if ok, err := path.Match(k.Scenario, id); err == nil && ok {
				matched = true
				complete = complete && r.ran[id]
			}
		}
		if !matched || complete {
			out = append(out, k)
		}
	}
	return out
}
