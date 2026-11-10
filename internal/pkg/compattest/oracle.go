//go:build compare

package compattest

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// OracleRuns is how many inputs an oracle test tries.
var OracleRuns = flag.Int("oracle.runs", 3000, "inputs an oracle test compares with the desktop client")

// Oracle is the desktop client's pure functions, running: tests ask it what
// the desktop answers for an input and compare that with their own answer.
// It exists only in the comparison image with the desktop's bundle (make
// compare-oracle); see tests/compare/js/pure/oracle.mjs.
type Oracle struct {
	t   testing.TB
	in  io.Writer
	out *bufio.Scanner
}

// Bytes passes b to the oracle as bytes instead of a string.
type Bytes []byte

// MarshalJSON implements json.Marshaler.
func (b Bytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"hex": hex.EncodeToString(b)})
}

// NewOracle starts the oracle for the duration of the test.
func NewOracle(t testing.TB) *Oracle {
	t.Helper()
	dist, js := os.Getenv("GROKBOT_COMPARE_DESKTOP"), os.Getenv("COMPARE_JS")
	if dist == "" || js == "" {
		t.Fatal("oracle tests run in the comparison image with the desktop's bundle: use make compare-oracle")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", filepath.Join(js, "pure", "oracle.mjs"), dist) //nolint:gosec // the oracle of the image
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		cmd.Wait() //nolint:errcheck // killed
	})
	scanner := bufio.NewScanner(out)
	scanner.Buffer(nil, 64<<20)
	return &Oracle{t: t, in: in, out: scanner}
}

// Call asks the oracle for fn(args...) and decodes the answer into result.
func (o *Oracle) Call(result any, fn string, args ...any) {
	o.t.Helper()
	if args == nil {
		args = []any{}
	}
	line, err := json.Marshal(map[string]any{"fn": fn, "args": args})
	if err != nil {
		o.t.Fatal(err)
	}
	if _, err := o.in.Write(append(line, '\n')); err != nil {
		o.t.Fatal(err)
	}
	if !o.out.Scan() {
		o.t.Fatalf("the oracle ended: %v", o.out.Err())
	}
	var answer struct {
		Result json.RawMessage
		Error  *string
	}
	if err := json.Unmarshal(o.out.Bytes(), &answer); err != nil {
		o.t.Fatalf("oracle answer %.200s: %v", o.out.Bytes(), err)
	}
	if answer.Error != nil {
		o.t.Fatalf("oracle %s: %s", fn, *answer.Error)
	}
	if err := json.Unmarshal(answer.Result, result); err != nil {
		o.t.Fatalf("oracle %s answered %.200s: %v", fn, answer.Result, err)
	}
}
