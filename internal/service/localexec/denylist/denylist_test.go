package denylist

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// The commands found in a command and the answer of the denylist are the
// desktop's, on the cases of the reference data; the oracle test compares
// the two on random commands and rules.
func TestReferenceDenylist(t *testing.T) {
	ref := compattest.Load(t)
	require.NotEmpty(t, ref.Denylist)
	for _, c := range ref.Denylist {
		found, ok := commands(c.Command)
		require.True(t, ok, c.Command)
		want := append([]string{}, c.Commands...)
		found = append([]string{}, found...)
		slices.Sort(want)
		slices.Sort(found)
		assert.Equal(t, slices.Compact(want), slices.Compact(found), c.Command)
		reason := ""
		if c.Reason != nil {
			reason = *c.Reason
		}
		assert.Equal(t, reason, Refusal(c.Command, c.Rules), "%q with rules %q", c.Command, c.Rules)
	}
}

// A command in a form the desktop's grammar reads in its own way, or that
// is not bash, is refused whatever the rules are; without rules it runs.
func TestUnanalyzable(t *testing.T) {
	for _, command := range []string{
		"echo (", "echo 'open", "[ -f x", "! { ls; }", "! for i in a; do ls; done", "time { ls; }", "coproc NAME { ls; }",
		"<<EOF cat\nbody\nEOF", "<<EOF\nbody\nEOF", "ls |& cat\necho next", "A=1 B=2\n! cat x", "A=1 B=2\n{ ls; }", "unset", "export",
		"unset A > out", "! A=1 ls", "", "unset A", "export A=1", "A=1",
	} {
		assert.Equal(t, Unanalyzable, Refusal(command, []string{"zz"}), command)
		assert.Empty(t, Refusal(command, nil), command)
	}
	// The same forms where they are read.
	for _, command := range []string{
		"[ -f x ] && ls", "! ls", "! [ -f x ] && ls", "time ls", "coproc sleep 1", "cat <<EOF\nbody\nEOF", "ls |& cat", "A=1 B=2\nls",
		"A=1\n! cat x", "unset A; ls", "export A=1 && ls", "> out export A=1", "ls 2>&1 | cat\necho next",
	} {
		assert.Empty(t, Refusal(command, []string{"zz"}), command)
	}
}

// Several substitutions in backquotes are reported one by one and as the
// desktop's grammar takes them, as one.
func TestBackquotes(t *testing.T) {
	found, ok := commands("echo `date` x `id -u`")
	require.True(t, ok)
	assert.Equal(t, []string{"echo `date` x `id -u`", "date` x `id -u", "date", "id -u"}, found)
}
