// Package compattest loads the reference outputs of the desktop client's pure
// functions from testdata/compat/<Version>/. Only tests import it; see
// testdata/compat/<Version>/README.md for how the data is produced.
package compattest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Version is the desktop release the reference data comes from.
const Version = "0.78.1"

// PureFunctions is pure-functions.json.
type PureFunctions struct {
	Provenance struct {
		Version  string
		Platform string
		Source   string
	}
	Ranges           []Range
	Streaming        []Streaming
	Fallback         []Fallback
	DiscoveryMissing json.RawMessage
	TerminalHeader   string
}

// Range is a read with a line range applied to a short text.
type Range struct {
	Text   string
	Args   json.RawMessage // agent.v1.ReadArgs without the path
	Result struct {
		Content      string
		RangeApplied bool
	}
}

// Streaming is a read through the desktop's streaming text reader. Long texts
// are stored as runs of one character.
type Streaming struct {
	Text     string
	TextRuns Runs
	Args     json.RawMessage // agent.v1.ReadArgs without the path
	Result   struct {
		Content      string
		ContentRuns  Runs
		TotalLines   int32
		Truncated    bool
		RangeApplied bool
	}
}

// Input returns the text that was read.
func (s Streaming) Input() string {
	if s.TextRuns != nil {
		return s.TextRuns.String()
	}
	return s.Text
}

// Content returns the content the reader returned.
func (s Streaming) Content() string {
	if s.TextRuns != nil {
		return s.Result.ContentRuns.String()
	}
	return s.Result.Content
}

// Fallback is the decoding of bytes that are not valid UTF-8.
type Fallback struct {
	Hex        string
	MostlyUtf8 bool
	Lossy      string
}

// Runs is a text stored as runs of one character.
type Runs []struct {
	Char  string
	Count int
}

func (r Runs) String() string {
	var b strings.Builder
	for _, run := range r {
		b.WriteString(strings.Repeat(run.Char, run.Count))
	}
	return b.String()
}

// Load reads pure-functions.json of Version and fails the test if it is
// missing, malformed or recorded from another version.
func Load(t testing.TB) *PureFunctions {
	t.Helper()
	b, err := os.ReadFile(Path("pure-functions.json"))
	if err != nil {
		t.Fatal(err)
	}
	var p PureFunctions
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatal(err)
	}
	if p.Provenance.Version != Version {
		t.Fatalf("pure-functions.json is from %q, want %q", p.Provenance.Version, Version)
	}
	return &p
}

// Path returns the path of a file in testdata/compat/<Version>/.
func Path(name string) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "compat", Version, name)
}
