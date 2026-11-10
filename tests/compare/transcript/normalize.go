package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	_ "image/gif"  // formats a read may answer with
	_ "image/jpeg" // formats a read may answer with
	_ "image/png"  // formats a read may answer with
	"regexp"
	"sort"
	"strconv"
	"strings"

	_ "golang.org/x/image/webp" // formats a read may answer with
)

// Options is what the normalizer needs to know about a run.
type Options struct {
	// Paths are replaced by their names wherever they start a path in a
	// string, e.g. {"/work/run/x/root": "$ROOT"}.
	Paths map[string]string
	// OrderedOutput keeps the order of stdout and stderr chunks of a shell
	// command. Without it the chunks of each stream are joined: how a
	// command's output is split and interleaved depends on timing.
	OrderedOutput bool
	// KeepAlive keeps the frames a client sends only to show that a request
	// is alive: one of each kind for every run of them.
	KeepAlive bool
	// Rules are applied after the built-in ones.
	Rules []Rule
}

// Rule rewrites the values at the paths Path matches. Path is a slash
// separated pattern over the compared value ("requests/r1/0/client/id"): "*"
// matches one segment, "**" any number of them. With a Pattern, the matches
// in a string value are replaced by Replace; with a Func, the value is
// replaced by what it returns; with neither, by Replace.
type Rule struct {
	Path    string
	Pattern *regexp.Regexp
	Replace string
	Func    func(any) any
}

// builtin are the values that differ between two runs of the same client:
// times, durations and process IDs. Each rule names the field it is for, so
// that a rule does not hide a difference elsewhere.
var builtin = []Rule{
	{Path: "requests/*/*/client/localExecutionTimeMs", Replace: "<MS>"},
	{Path: "requests/*/*/client/shellStream/exit/localExecutionTimeMs", Replace: "<MS>"},
	{Path: "requests/*/*/client/shellStream/backgrounded/pid", Replace: "<PID>"},
	{Path: "requests/*/*/client/backgroundShellSpawnResult/success/pid", Replace: "<PID>"},
	{Path: "requests/**/terminalMetadata/lastModifiedMs", Replace: "<TS>"},
	{Path: "requests/**/terminalMetadata/*/timestampMs", Replace: "<TS>"},
	{Path: "requests/**/terminalMetadata/*/durationMs", Replace: "<MS>"},
	{Path: "requests/**/terminalMetadata/lastCommands/*/timestampMs", Replace: "<TS>"},
	{Path: "requests/**/terminalMetadata/lastCommands/*/durationMs", Replace: "<MS>"},
	// A JavaScript stack has frames of the desktop's bundle; its first line,
	// the error, is what a client can repeat.
	{Path: "requests/*/*/control/throw/stackTrace", Pattern: regexp.MustCompile(`(?s)\n {4}at .*`)},
	// The desktop writes a file under a temporary name with a random suffix
	// and renames it; its errors name that file.
	{Path: "requests/*/*/fileError/error", Pattern: regexp.MustCompile(`\.\d{6,10}'`), Replace: ".<TMP>'"},
}

// Normalize rewrites t in place so that two runs of the same behavior give
// the same transcript.
func Normalize(t *Transcript, o Options) {
	// The identifiers are numbered in the order the answers mention them:
	// requests by ID, then the files.
	requests := make(map[string]any, len(t.Requests))
	for id, frames := range t.Requests {
		frames = keepAlive(frames, o.KeepAlive)
		requests[id] = joinOutput(frames, o.OrderedOutput)
	}
	n := &normalizer{
		ids:   &identifiers{shells: map[string]string{}, uuids: map[string]string{}},
		rules: append(append([]Rule(nil), builtin...), o.Rules...),
		paths: pathReplacer(o.Paths),
	}
	for id, frames := range object(n.rewrite("requests", requests)) {
		t.Requests[id] = list(frames)
	}
	if t.Files != nil {
		t.Files = object(n.rewrite("files", t.Files))
	}
	if t.Hello != nil {
		t.Hello = n.rewrite("hello", t.Hello)
	}
	if t.Calls != nil {
		t.Calls = list(n.rewrite("calls", t.Calls))
	}
	if t.Headers != nil {
		t.Headers = object(n.rewrite("headers", t.Headers))
	}
}

type normalizer struct {
	ids   *identifiers
	rules []Rule
	paths func(string) string
}

// rewrite normalizes one part of a transcript; root is its key.
func (n *normalizer) rewrite(root string, v any) any {
	v = walk(terminals(n.ids.replace(generic(v))), []string{root}, n.visit)
	return shorten(pictures(v, root))
}

// visit applies the rules to one node.
func (n *normalizer) visit(path []string, node any) (any, bool) {
	_, isObject := node.(map[string]any)
	_, isList := node.([]any)
	for _, r := range n.rules {
		if !matchPath(strings.Split(r.Path, "/"), path) {
			continue
		}
		s, isString := node.(string)
		switch {
		case r.Func != nil:
			return r.Func(node), true
		case isObject || isList:
		case r.Pattern == nil:
			node = r.Replace
		case isString:
			node = r.Pattern.ReplaceAllString(s, r.Replace)
		}
	}
	if s, ok := node.(string); ok {
		node = n.paths(s)
	}
	return node, false
}

// object returns v as a JSON object, nil if it is none.
func object(v any) map[string]any {
	m, _ := v.(map[string]any) //nolint:errcheck // nil is the answer
	return m
}

// list returns v as a JSON list, nil if it is none.
func list(v any) []any {
	l, _ := v.([]any) //nolint:errcheck // nil is the answer
	return l
}

// longText is the length from which a string is described instead of kept:
// megabytes of output do not belong in a transcript.
const (
	longText = 8 << 10
	textEdge = 256
)

// shorten replaces long strings by their length, hash, start and end.
func shorten(v any) any {
	return walk(v, nil, func(_ []string, node any) (any, bool) {
		s, ok := node.(string)
		if !ok || len(s) < longText {
			return node, false
		}
		sum := sha256.Sum256([]byte(s))
		return map[string]any{
			"length": json.Number(strconv.Itoa(len(s))), "sha256": hex.EncodeToString(sum[:]),
			"start": strings.ToValidUTF8(s[:textEdge], ""), "end": strings.ToValidUTF8(s[len(s)-textEdge:], ""),
		}, true
	})
}

// keepAlive removes the frames that only show a request is alive: exec
// heartbeats and messages_accepted. With keep, every run of them becomes one
// frame of each kind it has, messages_accepted first: how many a client sends
// and how the two kinds interleave depends on how long it waited.
func keepAlive(frames []any, keep bool) []any {
	out := make([]any, 0, len(frames))
	var accepted, heartbeat any // of the run that is open
	flush := func() {
		for _, f := range []any{accepted, heartbeat} {
			if f != nil && keep {
				out = append(out, f)
			}
		}
		accepted, heartbeat = nil, nil
	}
	for _, f := range frames {
		_, isAccepted := object(f)["messagesAccepted"]
		_, isHeartbeat := object(object(f)["control"])["heartbeat"]
		switch {
		case isAccepted && accepted == nil:
			accepted = f
		case isHeartbeat && heartbeat == nil:
			heartbeat = f
		case isAccepted || isHeartbeat:
		default:
			flush()
			out = append(out, f)
		}
	}
	flush()
	return out
}

// outputChunk returns the stream ("stdout" or "stderr") and text of a shell
// output frame.
func outputChunk(frame any) (stream, data string, ok bool) {
	shell := object(object(object(frame)["client"])["shellStream"])
	for _, stream := range []string{"stdout", "stderr"} {
		if chunk, ok := shell[stream].(map[string]any); ok {
			data, _ := chunk["data"].(string) //nolint:errcheck // "" for a chunk without data
			return stream, data, true
		}
	}
	return "", "", false
}

// withOutput returns a copy of an output frame carrying data.
func withOutput(frame any, stream, data string) any {
	out := map[string]any{}
	for k, v := range object(object(frame)["client"]) {
		out[k] = v
	}
	out["shellStream"] = map[string]any{stream: map[string]any{"data": data}}
	return map[string]any{"client": out}
}

// joinOutput joins the output chunks of every run of output frames: adjacent
// chunks of one stream if ordered, else all of stdout followed by all of
// stderr.
func joinOutput(frames []any, ordered bool) []any {
	out := make([]any, 0, len(frames))
	for i := 0; i < len(frames); {
		if _, _, ok := outputChunk(frames[i]); !ok {
			out = append(out, frames[i])
			i++
			continue
		}
		end := i
		for end < len(frames) {
			if _, _, ok := outputChunk(frames[end]); !ok {
				break
			}
			end++
		}
		out = append(out, joinRun(frames[i:end], ordered)...)
		i = end
	}
	return out
}

func joinRun(run []any, ordered bool) []any {
	var out []any
	if ordered {
		for _, f := range run {
			stream, data, _ := outputChunk(f)
			if n := len(out); n > 0 {
				if last, text, _ := outputChunk(out[n-1]); last == stream {
					out[n-1] = withOutput(f, stream, text+data)
					continue
				}
			}
			out = append(out, withOutput(f, stream, data))
		}
		return out
	}
	for _, want := range []string{"stdout", "stderr"} {
		var text strings.Builder
		var first any
		for _, f := range run {
			if stream, data, _ := outputChunk(f); stream == want {
				if first == nil {
					first = f
				}
				text.WriteString(data)
			}
		}
		if first != nil {
			out = append(out, withOutput(first, want, text.String()))
		}
	}
	return out
}

// pathReplacer returns a function that replaces the given paths where they
// start a path in a string: at the start, or after a character that cannot
// be part of the path before it. Longer paths win.
func pathReplacer(paths map[string]string) func(string) string {
	keys := make([]string, 0, len(paths))
	for k := range paths {
		if k != "" {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	patterns := make([]*regexp.Regexp, len(keys))
	for i, k := range keys {
		patterns[i] = regexp.MustCompile(regexp.QuoteMeta(k) + `($|[^A-Za-z0-9_-])`)
	}
	return func(s string) string {
		for i, k := range keys {
			if strings.Contains(s, k) {
				s = patterns[i].ReplaceAllString(s, strings.ReplaceAll(paths[k], "$", "$$")+"${1}")
			}
		}
		return s
	}
}

// walk rebuilds v top down: visit gets every node with its path and returns
// what replaces it and whether that is final; a node that is not final and
// is an object or a list is replaced by the walk of its members.
func walk(v any, path []string, visit func([]string, any) (any, bool)) any {
	v, done := visit(path, v)
	if done {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = walk(e, append(path[:len(path):len(path)], k), visit)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = walk(e, append(path[:len(path):len(path)], itoa(i)), visit)
		}
		return out
	}
	return v
}

// matchPath reports whether the segments of path match pattern.
func matchPath(pattern, path []string) bool {
	if len(pattern) == 0 {
		return len(path) == 0
	}
	if pattern[0] == "**" {
		for i := 0; i <= len(path); i++ {
			if matchPath(pattern[1:], path[i:]) {
				return true
			}
		}
		return false
	}
	if len(path) == 0 || (pattern[0] != "*" && pattern[0] != path[0]) {
		return false
	}
	return matchPath(pattern[1:], path[1:])
}

// pictures replaces the bytes a read answers with by what they are: an
// image by its format and size, since each client encodes images itself,
// and other bytes by their size and hash.
func pictures(v any, root string) any {
	return walk(v, []string{root}, func(path []string, node any) (any, bool) {
		s, ok := node.(string)
		if !ok || !matchPath([]string{"requests", "*", "*", "client", "readResult", "success", "data"}, path) {
			return node, false
		}
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return node, false
		}
		if config, format, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			return map[string]any{
				"image": format, "width": json.Number(strconv.Itoa(config.Width)), "height": json.Number(strconv.Itoa(config.Height)),
			}, true
		}
		return Blob(data), true
	})
}

// Bytes describes base64 data the way Blob describes bytes; a value that is
// not base64 is returned as it is.
func Bytes(encoded string) any {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return encoded
	}
	return Blob(data)
}
