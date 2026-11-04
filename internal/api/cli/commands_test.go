package cli_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/api/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/app"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := cli.NewRoot(app.App{})
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

func TestVersion(t *testing.T) {
	out, err := run(t, "--version")
	if err != nil || out != "grokbot-tunnel dev (Sand protocol 0.78.4)\n" {
		t.Fatal(out, err)
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"bogus"}, "unknown command"},
		{[]string{"connect", "--permission", "sometimes"}, `invalid permission "sometimes"`},
		{[]string{"connect", "extra"}, "unknown command"},
		{[]string{"connect", "--label", " \t "}, "--label must not be blank"},
		{[]string{"install", "--permission", "always", "--label", " "}, "--label must not be blank"},
		{[]string{"login", "--team", "-1"}, "team must be a positive integer"},
		{[]string{"install"}, "--permission is required"},
		{[]string{"install", "--permission", "sometimes"}, `invalid permission "sometimes"`},
		{[]string{"install", "extra", "--permission", "always"}, "unknown command"},
		{[]string{"uninstall", "extra"}, "unknown command"},
		{[]string{"update", "newest"}, `invalid version "newest"`},
		{[]string{"update", "v1", "v2"}, "accepts at most 1 arg"},
	} {
		if _, err := run(t, tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: got %v, want %q", tc.args, err, tc.want)
		}
	}
}

func TestMissingCommand(t *testing.T) {
	if _, err := run(t); err == nil || !strings.Contains(err.Error(), "missing command") {
		t.Fatal(err)
	}
}
