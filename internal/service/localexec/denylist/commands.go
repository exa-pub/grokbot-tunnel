package denylist

import (
	"regexp"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// commands returns the simple commands in command as the desktop's parser
// reports them: the name and the arguments of each, as written, joined by
// single spaces. ok is false if the command does not parse or has a form
// that the grammar the desktop uses is known to read in its own way and
// that is not followed here; such a command is refused.
//
// That grammar is not bash's. Where the two differ on commands that mean
// something, the desktop's reading is followed:
//
//   - "time", "coproc" and "let" are commands, and what follows them, an
//     assignment included, their arguments;
//   - "unset", "unsetenv" and the declaration builtins (declare, typeset,
//     export, readonly, local) are not commands, unless they follow "!", an
//     assignment or a redirection; "[ … ]" is not one either. What is
//     substituted in their arguments is looked at all the same;
//   - a word after a redirection of a command belongs to the redirection,
//     not to the command's arguments, except after a here-string.
//
// Several substitutions in backquotes among the arguments of a command are
// one command to that grammar; they are reported both ways, so that a rule
// that matches either blocks (see backquotes). What is substituted in
// backquotes inside a here-document the desktop does not look at;
// grokbot-tunnel does.
//
// Forms that the grammar is known to take apart are not followed but
// refused: a "[" that is not closed, a "!" before anything but a simple
// command, a declaration, a test or a subshell, "time" and "coproc" before
// a compound command, a here-document before its command or without one, a
// line break after a "|&" pipeline, a line of several assignments before a
// line that does not start with a simple command, and "unset" or a
// declaration builtin without arguments or with a redirection after it.
func commands(command string) (found []string, ok bool) {
	file, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, false
	}
	w := walker{source: command, wrapped: map[*syntax.Stmt]bool{}, negated: map[*syntax.Stmt]bool{}}
	syntax.Walk(file, w.visit)
	return w.found, !w.unknown
}

type walker struct {
	source string
	found  []string
	// wrapped are the statements whose command "time" or "coproc" runs:
	// they are reported with it.
	wrapped map[*syntax.Stmt]bool
	// negated are the statements a "!" stands before: the first of a
	// pipeline or list, to the desktop's grammar.
	negated map[*syntax.Stmt]bool
	// assigned is the last statement of two or more assignments and
	// nothing else, until the statement after it is seen.
	assigned *syntax.Stmt
	// unknown is set by a form whose reading by the desktop is not known.
	unknown bool
}

// blankLines matches what separates two statements on lines of their own.
var blankLines = regexp.MustCompile(`^[ \t]*(#[^\n]*)?(\n[ \t]*(#[^\n]*)?)+$`)

func (w *walker) text(n syntax.Node) string { return w.source[n.Pos().Offset():n.End().Offset()] }

func (w *walker) add(words []string) { w.found = append(w.found, strings.Join(words, " ")) }

func (w *walker) visit(n syntax.Node) bool {
	switch x := n.(type) {
	case *syntax.BinaryCmd:
		if x.Op == syntax.PipeAll && strings.Contains(w.source[x.OpPos.Offset():], "\n") {
			// After "|&" the desktop's grammar does not end a command at
			// the end of its line.
			w.unknown = true
		}
	case *syntax.Stmt:
		w.afterAssignments(x)
		if x.Negated {
			w.negation(first(x))
		}
		w.statement(x)
	}
	return true
}

// statement reports the command of st, if it is one to the desktop.
func (w *walker) statement(st *syntax.Stmt) {
	switch cmd := st.Cmd.(type) {
	case nil:
		// Only redirections. A here-document without a command is read as
		// commands by the desktop.
		w.unknown = w.unknown || hasHeredoc(st.Redirs)
	case *syntax.CallExpr:
		if !w.wrapped[st] {
			w.call(st, cmd)
		}
	case *syntax.DeclClause:
		w.declaration(st, cmd)
	case *syntax.TimeClause:
		prefix := []string{"time"}
		if cmd.PosixFormat {
			prefix = append(prefix, "-p")
		}
		w.wrapper(st, prefix, cmd.Stmt)
	case *syntax.CoprocClause:
		if cmd.Name != nil {
			w.unknown = true // a named coprocess runs a compound command
		}
		w.wrapper(st, []string{"coproc"}, cmd.Stmt)
	case *syntax.LetClause:
		words := make([]string, 1, 1+len(cmd.Exprs))
		words[0] = "let"
		for _, expr := range cmd.Exprs {
			words = append(words, w.text(expr))
		}
		w.add(words)
	}
}

// afterAssignments looks at st, the statement seen after a line of several
// assignments: unless it starts with a simple command on the next line, the
// desktop's grammar takes the assignments for the start of it.
func (w *walker) afterAssignments(st *syntax.Stmt) {
	if before := w.assigned; before != nil {
		w.assigned = nil
		if end, start := before.End().Offset(), st.Pos().Offset(); end <= start && blankLines.MatchString(w.source[end:start]) {
			lead := first(st)
			call, simple := lead.Cmd.(*syntax.CallExpr)
			w.unknown = w.unknown || !simple || len(call.Args) == 0 || st.Negated || lead.Negated
		}
	}
	if call, ok := st.Cmd.(*syntax.CallExpr); ok && len(call.Args) == 0 && len(call.Assigns) > 1 {
		w.assigned = st
	}
}

// first returns the statement st starts with: itself, or the first of its
// pipeline or list.
func first(st *syntax.Stmt) *syntax.Stmt {
	for {
		binary, ok := st.Cmd.(*syntax.BinaryCmd)
		if !ok {
			return st
		}
		st = binary.X
	}
}

// negation notes that a "!" stands before st. The desktop's grammar knows
// it before a simple command, a declaration, a test and a subshell; before
// anything else it reads keywords as commands.
func (w *walker) negation(st *syntax.Stmt) {
	w.negated[st] = true
	known := false
	switch cmd := st.Cmd.(type) {
	case *syntax.CallExpr:
		known = len(cmd.Assigns) == 0 && len(cmd.Args) > 0 && !redirected(st, cmd.Args[0].Pos().Offset())
	case *syntax.DeclClause:
		known = !redirected(st, cmd.Pos().Offset())
	case *syntax.TestClause, *syntax.Subshell:
		known = true
	}
	w.unknown = w.unknown || !known
}

// redirected reports whether a redirection of st precedes offset start.
func redirected(st *syntax.Stmt, start uint) bool {
	for _, r := range st.Redirs {
		if r.Pos().Offset() < start {
			return true
		}
	}
	return false
}

func hasHeredoc(redirs []*syntax.Redirect) bool {
	for _, r := range redirs {
		if r.Op == syntax.Hdoc || r.Op == syntax.DashHdoc {
			return true
		}
	}
	return false
}

// limit returns the offset of the first redirection of st at or after from
// that ends the arguments of its command, and whether there is one.
func limit(st *syntax.Stmt, from uint) (uint, bool) {
	at, found := uint(0), false
	for _, r := range st.Redirs {
		if pos := r.Pos().Offset(); r.Op != syntax.WordHdoc && pos >= from && (!found || pos < at) {
			at, found = pos, true
		}
	}
	return at, found
}

// plain reports whether nothing precedes the command of st, which starts at
// offset start: no "!" and no redirection.
func (w *walker) plain(st *syntax.Stmt, start uint) bool {
	return !w.negated[st] && !redirected(st, start)
}

// words returns the words of nodes that precede the first redirection of
// st after from.
func words[N syntax.Node](w *walker, st *syntax.Stmt, from uint, nodes []N) []string {
	end, limited := limit(st, from)
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if limited && n.Pos().Offset() >= end {
			break
		}
		out = append(out, w.text(n))
	}
	return out
}

func (w *walker) call(st *syntax.Stmt, call *syntax.CallExpr) {
	if len(call.Args) == 0 {
		return // only assignments
	}
	name := call.Args[0]
	start := name.Pos().Offset()
	if hasHeredoc(st.Redirs) && redirected(st, start) {
		w.unknown = true // a here-document before the command
		return
	}
	switch w.text(name) {
	case "[":
		// A test: not a command when it is closed. Any other use the
		// desktop's grammar takes apart.
		if len(call.Assigns) > 0 || len(st.Redirs) > 0 || len(call.Args) < 2 || w.text(call.Args[len(call.Args)-1]) != "]" {
			w.unknown = true
		}
		return
	case "unset", "unsetenv":
		if len(call.Args) == 1 || len(st.Redirs) > 0 && !redirected(st, start) {
			w.unknown = true // without a name or redirected, the grammar loses what follows
			return
		}
		if len(call.Assigns) == 0 && w.plain(st, start) {
			return
		}
	}
	args := words(w, st, name.End().Offset(), call.Args)
	w.add(args)
	w.backquotes(call.Args[:len(args)])
}

// backquotes reports what the desktop's grammar takes for one command
// where several substitutions in backquotes are among args: all from the
// first one's opening quote to the last one's closing quote.
func (w *walker) backquotes(args []*syntax.Word) {
	var quoted []*syntax.CmdSubst
	for _, arg := range args {
		syntax.Walk(arg, func(n syntax.Node) bool {
			if subst, ok := n.(*syntax.CmdSubst); ok && subst.Backquotes {
				quoted = append(quoted, subst)
				return false
			}
			return true
		})
	}
	if len(quoted) > 1 {
		w.found = append(w.found, strings.TrimSpace(w.source[quoted[0].Left.Offset()+1:quoted[len(quoted)-1].Right.Offset()]))
	}
}

// declaration reports a declaration builtin as a command where the
// desktop's grammar does: after "!" or a redirection.
func (w *walker) declaration(st *syntax.Stmt, decl *syntax.DeclClause) {
	if len(decl.Args) == 0 || len(st.Redirs) > 0 && !redirected(st, decl.Pos().Offset()) {
		w.unknown = true // as for "unset"
		return
	}
	if w.plain(st, decl.Pos().Offset()) {
		return
	}
	w.add(append([]string{decl.Variant.Value}, words(w, st, decl.Variant.End().Offset(), decl.Args)...))
}

// wrapper reports "time" or "coproc", in prefix, with the simple command
// that inner starts with as its arguments.
func (w *walker) wrapper(st *syntax.Stmt, prefix []string, inner *syntax.Stmt) {
	if inner == nil || inner.Cmd == nil {
		w.add(prefix)
		return
	}
	first := first(inner)
	call, ok := first.Cmd.(*syntax.CallExpr)
	if !ok || first.Negated || inner.Negated || !w.plain(st, st.Cmd.Pos().Offset()) {
		w.unknown = true
		return
	}
	w.wrapped[first] = true
	// Assignments and arguments, in the order they are written.
	nodes := make([]syntax.Node, 0, len(call.Assigns)+len(call.Args))
	for _, a := range call.Assigns {
		nodes = append(nodes, a)
	}
	for _, a := range call.Args {
		nodes = append(nodes, a)
	}
	w.add(append(prefix, words(w, first, st.Cmd.Pos().Offset(), nodes)...))
}
