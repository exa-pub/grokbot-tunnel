package transcript

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Identifiers a client makes up anew on every run: the IDs of background
// shells, which also name their terminal files, and the UUIDs in the names of
// output files. They are replaced by placeholders numbered in the order the
// answers mention them, so that the same answers give the same names.
var (
	uuidPattern     = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	terminalPattern = regexp.MustCompile(`terminals/(\d+)\.txt`)
)

type identifiers struct {
	shells map[string]string
	uuids  map[string]string
}

func (ids *identifiers) shell(id string) string {
	if _, ok := ids.shells[id]; !ok {
		ids.shells[id] = fmt.Sprintf("<SHELL#%d>", len(ids.shells)+1)
	}
	return ids.shells[id]
}

func (ids *identifiers) uuid(id string) string {
	if _, ok := ids.uuids[id]; !ok {
		ids.uuids[id] = fmt.Sprintf("<UUID#%d>", len(ids.uuids)+1)
	}
	return ids.uuids[id]
}

func (ids *identifiers) text(s string) string {
	s = terminalPattern.ReplaceAllStringFunc(s, func(m string) string {
		return "terminals/" + ids.shell(terminalPattern.FindStringSubmatch(m)[1]) + ".txt"
	})
	return uuidPattern.ReplaceAllStringFunc(s, ids.uuid)
}

// replace rewrites the identifiers in v, visiting objects in key order so
// that the numbering does not depend on map order.
func (ids *identifiers) replace(v any) any {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(x))
		for _, k := range keys {
			if n, ok := x[k].(json.Number); ok && k == "shellId" {
				out[k] = ids.shell(n.String())
				continue
			}
			out[ids.text(k)] = ids.replace(x[k])
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = ids.replace(e)
		}
		return out
	case string:
		return ids.text(x)
	}
	return v
}

// Lines of a terminal file's header and footer that differ from run to run.
var (
	terminalHeader  = regexp.MustCompile(`\A---\n(pid: \d+\n)?cwd: `)
	terminalPid     = regexp.MustCompile(`(?m)^pid: \d+$`)
	terminalRunning = regexp.MustCompile(`(?m)^running_for_ms: \d+ *$`)
	terminalElapsed = regexp.MustCompile(`(?m)^elapsed_ms: \d+$`)
	terminalTime    = regexp.MustCompile(`(?m)^(started_at|ended_at): \d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)
)

// isTerminal reports whether s is the content of a terminal file: it starts
// with the header, which has no pid when the command never started.
func isTerminal(s string) bool { return terminalHeader.MatchString(s) }

// terminalText replaces the process ID, times and durations of a terminal
// file. The running time keeps its width: the field is padded so that it can
// be rewritten in place, and the padding is behavior.
func terminalText(s string) string {
	s = terminalPid.ReplaceAllString(s, "pid: <PID>")
	s = terminalRunning.ReplaceAllStringFunc(s, func(m string) string {
		const name = "running_for_ms: "
		width := len(m) - len(name)
		return name + "<MS>" + strings.Repeat(" ", max(0, width-len("<MS>")))
	})
	s = terminalElapsed.ReplaceAllString(s, "elapsed_ms: <MS>")
	return terminalTime.ReplaceAllString(s, "$1: <TS>")
}

// terminals rewrites the terminal file contents in v: in strings, and in
// descriptions of bytes (see Blob), whose size and hash then mean nothing.
func terminals(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		text, isText := x["text"].(string)
		for k, e := range x {
			if isText && isTerminal(text) && (k == "size" || k == "sha256") {
				continue
			}
			out[k] = terminals(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = terminals(e)
		}
		return out
	case string:
		if isTerminal(x) {
			return terminalText(x)
		}
	}
	return v
}
