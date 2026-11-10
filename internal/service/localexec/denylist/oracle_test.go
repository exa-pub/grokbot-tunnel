//go:build compare

package denylist

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// What the random commands are made of. The wild lists add what is seldom
// or never written: builtins in odd places, tests that are not closed,
// redirections before a command and text that is not bash.
var (
	names = []string{
		"rm", "echo", "git", "/bin/rm", `r\m`, "'rm'", `"rm"`, `$'r\x6d'`, "sudo", "env", "cat", "xargs", "ls", "command", "exec", "eval",
		"$cmd", "./run.sh", "make", "cd", "test", "time", "time -p",
	}
	wildNames = []string{"unset", "export", "[", "let", "local", "declare", "readonly", "unsetenv", "coproc", "]", "!", "{", "}"}
	arguments = []string{
		"-rf", "x", `"a b"`, "'c d'", "$HOME", "${X:-y}", "$(ls -la)", "`date`", "<(sort f)", "a*b", "--force", "origin/main", "FOO=bar",
		"push", "-f", "/", "~", `a\ b`, `$'t\x41b'`, `"$(rm y)"`, "1", "x=1+2", "--", "$((1+2))", "{a,b}", "a=b=c",
	}
	wildArguments = []string{"]", `"`, "'", "[", ";;", "then", "fi"}
	redirections  = []string{"> out", "2>&1", "< in", ">> log", "<<< str", "&> all", "2> /dev/null", ">out", ">&2"}
	// statements are whole commands of the forms the desktop's grammar
	// does not take for simple commands, written as they usually are.
	statements = []string{
		"[ -f x ]", `[ "$a" = b ]`, "[[ -f x ]]", "[[ $a == b* ]]", "unset A B", "export A=1", "export PATH=$PATH:$(pwd)", "local x=1",
		"declare -a arr", "readonly R=1", "let x=1+2", "(( x++ ))", "x=$(rm y)", "A=1 B=2", "time make -j4", "coproc sleep 1",
		"unset -f f", "typeset -i n=1", "x=1 time ls", "A=1 unset B", "> out export A=1", "time A=1 ls -la > out", "let x=1 y=2",
		"[ -n \"$(rm y)\" ]",
	}
	// heredocs end a command: what follows a here-document starts on a
	// new line.
	heredocs    = []string{"cat <<EOF\nbody $(rm z)\nEOF", "cat <<'EOF' > out\nbody $(rm z)\nEOF", "sudo tee x <<-EOF | xargs\n\tbody $(date)\n\tEOF"}
	joiners     = []string{" ; ", " && ", " || ", " | ", " & ", "\n", ";"}
	wildJoiners = []string{" |& ", "  ", ";;"}
	rules       = []string{
		"rm", "rm -rf", "git", "git push", "git:*--force*", "*", "**", ":x", "a:b c", "r*", "*rm*", "sudo", "ls", "echo", "time",
		"  rm  ", "", "rm\t-f", "xargs:*", "*:*", "e*o:*", "/bin/rm", "cat", "unset", "export", "[", "let", "rm:", "rm : -rf*",
		"*:x", "git:push*", "ls -la", "date", "sort", "rm y", "rm z", "x", "$(ls", "echo*x", "a b", "body", "make", "sleep", "coproc",
		strings.Repeat("r", 513), strings.Repeat("r", 512), "r\u00a0m", "rm\n-rf",
	}
)

func pick(r *rand.Rand, lists ...[]string) string {
	n := 0
	for _, list := range lists {
		n += len(list)
	}
	i := r.IntN(n)
	for _, list := range lists {
		if i < len(list) {
			return list[i]
		}
		i -= len(list)
	}
	panic("unreachable")
}

func randomSimple(r *rand.Rand, wild bool) string {
	if !wild && r.IntN(5) == 0 {
		return pick(r, statements)
	}
	var b strings.Builder
	if r.IntN(6) == 0 {
		b.WriteString("A=1 ")
	}
	if wild && r.IntN(8) == 0 {
		b.WriteString(pick(r, redirections, []string{"<<EOF\nbody $(rm z)\nEOF\n"}) + " ")
	}
	if wild {
		b.WriteString(pick(r, names, wildNames))
	} else {
		name := pick(r, names)
		// A "!" before a simple command; before anything else the
		// desktop's grammar reads it in ways that are not followed.
		if r.IntN(12) == 0 && b.Len() == 0 && !strings.HasPrefix(name, "time") {
			b.WriteString("! ")
		}
		b.WriteString(name)
	}
	backquoted := false
	for range r.IntN(4) {
		b.WriteString(" ")
		switch {
		case r.IntN(5) == 0:
			b.WriteString(pick(r, redirections))
		case wild:
			b.WriteString(pick(r, arguments, wildArguments))
		default:
			// The desktop's grammar takes two substitutions in backquotes
			// for one; see walker.backquotes.
			arg := pick(r, arguments)
			for backquoted && strings.Contains(arg, "`") {
				arg = pick(r, arguments)
			}
			backquoted = backquoted || strings.Contains(arg, "`")
			b.WriteString(arg)
		}
	}
	return b.String()
}

// randomUsual is a command as it is usually written, at times with a
// here-document at its end.
func randomUsual(r *rand.Rand) string {
	command := randomCommand(r, 2, false)
	if r.IntN(6) == 0 {
		command += pick(r, []string{" && ", "\n", " ; "}) + pick(r, heredocs)
	}
	return command
}

func randomCommand(r *rand.Rand, depth int, wild bool) string {
	var b strings.Builder
	piped := false
	for i := range 1 + r.IntN(3) {
		if i > 0 {
			joiner := pick(r, joiners)
			if wild {
				joiner = pick(r, joiners, wildJoiners)
			}
			piped = strings.Contains(joiner, "|") && !strings.Contains(joiner, "||")
			b.WriteString(joiner)
		}
		inner := randomSimple(r, wild)
		if depth > 0 && r.IntN(3) == 0 {
			inner = randomCommand(r, depth-1, wild)
		}
		switch r.IntN(16) {
		case 0:
			inner = "( " + inner + " )"
		case 1:
			inner = "{ " + inner + "; }"
		case 2:
			inner = "if " + inner + "; then " + randomSimple(r, wild) + "; fi"
		case 3:
			inner = "for i in a b; do " + inner + "; done"
		case 4:
			inner = "f() { " + inner + "; }"
		case 5:
			inner = "while " + inner + "; do " + randomSimple(r, wild) + "; done"
		case 6:
			if wild {
				inner += " # rm x"
			}
		case 7:
			inner = "case $x in a) " + inner + " ;; esac"
		case 8:
			if wild {
				// Something that is not bash.
				inner += pick(r, []string{" (", " )", " '", ` "`, " `", " $(", " ;;", " |", " &&", " fi", " }"})
			}
		case 9:
			if wild {
				inner = "! " + inner
			}
		}
		if !wild && piped && strings.HasPrefix(inner, "! ") {
			inner = inner[2:] // a "!" starts a pipeline
		}
		b.WriteString(inner)
	}
	return b.String()
}

type verdict struct {
	Reason        *string
	Commands      []string
	ParsingFailed bool
}

// The rules of a denylist match as the desktop's do: given the simple
// commands the desktop found in a random command, grokbot-tunnel's matching
// gives the desktop's answer for random rules.
func TestOracleDenylistRules(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(9, 10))
	for range *compattest.OracleRuns {
		command := randomCommand(r, 1, true)
		list := make([]string, r.IntN(4))
		for i := range list {
			list[i] = pick(r, rules)
		}
		var want verdict
		oracle.Call(&want, "denylist", command, list)
		if want.ParsingFailed || len(want.Commands) == 0 || len(list) == 0 {
			continue
		}
		got := ""
		if rule, blocked := blockingRule(append([]string{command}, want.Commands...), list); blocked {
			got = blockedBy(rule)
		}
		if desktop := deref(want.Reason); got != desktop {
			t.Errorf("rules %q on %q with commands %q\n got     %q\n desktop %q", list, command, want.Commands, got, desktop)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// In commands as they are usually written, the simple commands found are
// those the desktop finds, and the answer is the desktop's. A few in ten
// thousand the desktop's grammar reads in a way of its own that is not
// followed; none of them runs when the desktop refuses it.
func TestOracleDenylistCommands(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(11, 12))
	refused, different := 0, 0
	for range *compattest.OracleRuns {
		command := randomUsual(r)
		list := []string{pick(r, rules), pick(r, rules)}
		var want verdict
		oracle.Call(&want, "denylist", command, list)
		found, ok := commands(command)
		got, desktop := Refusal(command, list), deref(want.Reason)
		switch {
		case got == "" && desktop != "":
			t.Errorf("%q with rules %q runs; the desktop refuses it: %q\n commands %q\n desktop  %q", command, list, desktop, found, want.Commands)
		case !ok:
			refused++
			t.Logf("not read: %q", command)
		case !slices.Equal(sorted(want.Commands), sorted(found)):
			different++
			t.Logf("commands of %q\n got     %q\n desktop %q", command, found, want.Commands)
		case got != desktop:
			t.Errorf("%q with rules %q\n got     %q\n desktop %q", command, list, got, desktop)
		}
	}
	t.Logf("of %d usual commands, %d are refused as not analyzable and %d are read differently", *compattest.OracleRuns, refused, different)
	if refused > *compattest.OracleRuns/100 || different > *compattest.OracleRuns/1000 {
		t.Errorf("too many usual commands are refused (%d) or read differently (%d)", refused, different)
	}
}

// Whatever the command is, grokbot-tunnel does not run one the desktop
// refuses: what it reads differently, or not at all, it refuses.
func TestOracleDenylistNeverRuns(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(13, 14))
	same, stricter, different := 0, 0, 0
	for range *compattest.OracleRuns {
		command := randomCommand(r, 2, true)
		list := []string{pick(r, rules), pick(r, rules)}
		var want verdict
		oracle.Call(&want, "denylist", command, list)
		desktop := deref(want.Reason)
		got := Refusal(command, list)
		found, ok := commands(command)
		switch {
		case got == "" && desktop != "":
			t.Errorf("%q with rules %q runs; the desktop refuses it: %q\n commands %q\n desktop  %q", command, list, desktop, found, want.Commands)
		case !ok:
			stricter++
		case !slices.Equal(sorted(want.Commands), sorted(found)):
			different++
			if different <= 30 {
				t.Logf("commands of %q\n got     %q\n desktop %q", command, found, want.Commands)
			}
		default:
			same++
		}
	}
	t.Logf("%d read the same, %d refused as not analyzable, %d read differently", same, stricter, different)
}

func sorted(list []string) []string {
	out := slices.Clone(list)
	slices.Sort(out)
	return slices.Compact(out)
}
