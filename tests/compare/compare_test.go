//go:build compare

package compare

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/scenario"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/scenarios"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/subject"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/transcript"
)

var (
	record = flag.Bool("compare.record", false, "TestLive: write the desktop's transcripts as the new recordings")
	repeat = flag.Int("compare.repeat", 3, "TestLive with -compare.record: how many equal runs a recording needs")
)

// settings are where run.sh put things in the container.
type settings struct {
	tunnel   subject.Tunnel
	js       string // tests/compare/js
	testdata string // testdata/compat
	out      string // where reports and transcripts go
	work     string // where clients run
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func load(t *testing.T) settings {
	t.Helper()
	if os.Getenv("GROKBOT_COMPARE_IMAGE") == "" {
		t.Fatal("comparison tests run in their image: use make compare or make compare-golden")
	}
	s := settings{
		tunnel:   subject.Tunnel{Binary: env("COMPARE_TUNNEL", "/compare/bin/grokbot-tunnel")},
		js:       env("COMPARE_JS", "/compare/js"),
		testdata: env("COMPARE_TESTDATA", "/compare/testdata"),
		out:      env("COMPARE_OUT", "/out"),
		work:     env("COMPARE_WORK", "/work/run"),
	}
	if err := os.MkdirAll(s.out, 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// result is the outcome of one scenario, for the report.
type result struct {
	id                string
	known, unexpected []transcript.Difference
	// failed says the scenario failed for another reason than a difference:
	// a client that crashed, a stale recording.
	failed bool
}

type report struct {
	mu      sync.Mutex
	results []result
}

func (r *report) add(res result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.results = append(r.results, res)
}

// watch records a scenario that fails without reaching the comparison, or
// fails for another reason than what it compared.
func (r *report) watch(t *testing.T, id string) {
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		for i := range r.results {
			if r.results[i].id == id {
				r.results[i].failed = len(r.results[i].unexpected) == 0
				return
			}
		}
		r.results = append(r.results, result{id: id, failed: true})
	})
}

// TestLive runs every scenario against the desktop client and against
// grokbot-tunnel in the same container and compares the two. With
// -compare.record it also writes the desktop's transcripts as recordings.
func TestLive(t *testing.T) {
	s := load(t)
	version := os.Getenv("GROKBOT_COMPARE_DESKTOP_VERSION")
	desktop := subject.Desktop{Version: version, Dist: os.Getenv("GROKBOT_COMPARE_DESKTOP"), Launcher: filepath.Join(s.js, "launcher.mjs")}
	if desktop.Dist == "" {
		t.Fatal("no desktop bundle in this image: TestLive needs the desktop image (make compare)")
	}
	registry := known(t, s, version)
	rep := &report{}
	for _, sc := range scenarios.All() {
		t.Run(sc.ID, func(t *testing.T) {
			rep.watch(t, sc.ID)
			want := Run(t, s.work, sc, desktop)
			if *record {
				for i := 1; i < *repeat; i++ {
					again := Run(t, s.work, sc, desktop)
					if !reflect.DeepEqual(want.Compared(), again.Compared()) {
						save(t, filepath.Join(s.out, "unstable"), sc.ID+".1", want)
						save(t, filepath.Join(s.out, "unstable"), sc.ID+".2", again)
						t.Fatalf("the desktop's transcript differs between runs; not recorded: %s",
							describe(transcript.Diff(want.Compared(), again.Compared())))
					}
				}
				save(t, filepath.Join(s.out, "transcripts"), sc.ID, want)
			}
			got := Run(t, s.work, sc, s.tunnel)
			save(t, filepath.Join(s.out, "desktop"), sc.ID, want)
			save(t, filepath.Join(s.out, "go"), sc.ID, got)
			check(t, registry, rep, sc, want, got)
		})
	}
	finish(t, s, registry, rep, "desktop "+version+" (live)")
}

// TestGolden runs every scenario against grokbot-tunnel and compares it with
// the recorded transcript of the desktop client.
func TestGolden(t *testing.T) {
	s := load(t)
	registry := known(t, s, compattest.Version)
	rep := &report{}
	for _, sc := range scenarios.All() {
		t.Run(sc.ID, func(t *testing.T) {
			rep.watch(t, sc.ID)
			want := recorded(t, s, sc)
			got := Run(t, s.work, sc, s.tunnel)
			save(t, filepath.Join(s.out, "go"), sc.ID, got)
			for _, program := range []string{"base", "bash", "zsh", "rg", "git"} {
				if want.Env[program] != got.Env[program] {
					t.Fatalf("the environment is not the one of the recording: %s is %q, recorded with %q; "+
						"rebuild the image or record again (make compare-record)", program, got.Env[program], want.Env[program])
				}
			}
			check(t, registry, rep, sc, want, got)
		})
	}
	orphans(t, s)
	finish(t, s, registry, rep, "desktop "+compattest.Version+" (recorded)")
}

// orphans fails for recordings no scenario has: of a scenario that was
// removed or renamed.
func orphans(t *testing.T, s settings) {
	t.Helper()
	have := map[string]bool{}
	for _, sc := range scenarios.All() {
		have[sc.ID] = true
	}
	dir := filepath.Join(s.testdata, compattest.Version, "transcripts")
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if id := strings.TrimSuffix(filepath.ToSlash(rel), ".json"); !have[id] {
			t.Errorf("recording %s has no scenario; remove it or record again (make compare-record)", rel)
		}
		return nil
	})
	if err != nil {
		t.Error(err)
	}
}

// recorded loads the recording of a scenario and fails if there is none or
// it was made for another version of the scenario.
func recorded(t *testing.T, s settings, sc scenario.Scenario) *transcript.Transcript {
	t.Helper()
	file := filepath.Join(s.testdata, compattest.Version, "transcripts", sc.ID+".json")
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("no recording of the desktop for this scenario (make compare-record): %v", err)
	}
	want := &transcript.Transcript{}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(want); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if want.Digest != sc.Digest() {
		t.Fatalf("the recording is of another version of this scenario; record again (make compare-record)")
	}
	return want
}

func known(t *testing.T, s settings, version string) *transcript.Registry {
	t.Helper()
	file := filepath.Join(s.testdata, version, "known-diffs.json")
	if _, err := os.Stat(file); err != nil {
		// A desktop release we have not moved to yet is compared with what
		// is known about the current one.
		file = filepath.Join(s.testdata, compattest.Version, "known-diffs.json")
	}
	registry, err := transcript.LoadKnown(file)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func check(t *testing.T, registry *transcript.Registry, rep *report, sc scenario.Scenario, want, got *transcript.Transcript) {
	t.Helper()
	knownDiffs, unexpected := registry.Split(sc.ID, transcript.Diff(want.Compared(), got.Compared()))
	rep.add(result{id: sc.ID, known: knownDiffs, unexpected: unexpected})
	if len(unexpected) > 0 {
		t.Errorf("%d differences from the desktop: %s", len(unexpected), describe(unexpected))
	}
}

// finish fails on known differences that are gone and writes the report.
func finish(t *testing.T, s settings, registry *transcript.Registry, rep *report, against string) {
	t.Helper()
	all := make([]string, 0, len(scenarios.All()))
	for _, sc := range scenarios.All() {
		all = append(all, sc.ID)
	}
	stale := registry.Stale(all)
	for _, k := range stale {
		t.Errorf("known difference no longer there, remove it from known-diffs.json: %s %s (%s)", k.Scenario, k.Path, k.Reason)
	}
	sort.Slice(rep.results, func(i, j int) bool { return rep.results[i].id < rep.results[j].id })
	var b strings.Builder
	fmt.Fprintf(&b, "# grokbot-tunnel against %s\n\n", against)
	same, withKnown, differ, failed := 0, 0, 0, 0
	for _, r := range rep.results {
		switch {
		case r.failed:
			failed++
		case len(r.unexpected) > 0:
			differ++
		case len(r.known) > 0:
			withKnown++
		default:
			same++
		}
	}
	fmt.Fprintf(&b, "%d scenarios: %d the same, %d with known differences only, %d with new differences, %d failed otherwise. "+
		"%d stale known differences.\n\n", len(rep.results), same, withKnown, differ, failed, len(stale))
	b.WriteString("| Scenario | Result |\n|---|---|\n")
	for _, r := range rep.results {
		status := "same"
		switch {
		case r.failed:
			status = "**failed**, see the test output"
		case len(r.unexpected) > 0:
			status = fmt.Sprintf("**%d new**, %d known", len(r.unexpected), len(r.known))
		case len(r.known) > 0:
			status = fmt.Sprintf("%d known", len(r.known))
		}
		fmt.Fprintf(&b, "| `%s` | %s |\n", r.id, status)
	}
	for _, r := range rep.results {
		if len(r.unexpected) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n%s\n", r.id, describe(r.unexpected))
	}
	if err := os.WriteFile(filepath.Join(s.out, "report.md"), []byte(b.String()), 0o644); err != nil {
		t.Error(err)
	}
}

// describe lists differences for a person to read.
func describe(diffs []transcript.Difference) string {
	var b strings.Builder
	for _, d := range diffs {
		fmt.Fprintf(&b, "\n- `%s`\n  - desktop: %s\n  - go:      %s", d.Path, show(d.Desktop), show(d.Go))
	}
	return b.String()
}

func show(v *json.RawMessage) string {
	if v == nil {
		return "(nothing)"
	}
	const limit = 600
	s := string(*v)
	if len(s) > limit {
		s = fmt.Sprintf("%s… (%d bytes)", s[:limit], len(s))
	}
	return "`" + s + "`"
}

func save(t *testing.T, dir, id string, tr *transcript.Transcript) {
	t.Helper()
	file := filepath.Join(dir, id+".json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(tr); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}
