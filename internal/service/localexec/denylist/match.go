package denylist

import (
	"regexp"
	"slices"
	"strings"
	"unicode/utf16"
)

// The desktop's matching of a command against the rules of a denylist
// (its Zpe, vGe and KVt), function by function; the oracle test compares the
// two on random rules and commands.

// maxRule is how long a rule may be, in UTF-16 code units.
const maxRule = 512

// blockingRule returns the first of rules that blocks one of forms, the
// spellings of a command and of the commands in it.
func blockingRule(forms, rules []string) (string, bool) {
	rules = nonEmpty(rules)
	if len(rules) == 0 {
		return "", false
	}
	forms = nonEmpty(forms)
	if len(forms) == 0 {
		forms = []string{""}
	}
	for _, rule := range rules {
		if slices.ContainsFunc(forms, func(form string) bool { return blocks(rule, form) }) {
			return rule, true
		}
	}
	return "", false
}

// nonEmpty returns the strings of list, trimmed, that are not empty.
func nonEmpty(list []string) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		if s = jsTrim(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// blocks reports whether rule blocks command. A rule that is not valid
// blocks every command. The command is tried as written, with its whitespace
// collapsed, and with its quoting removed.
func blocks(rule, command string) bool {
	rule = jsTrim(collapse(rule))
	if rule == "" {
		return false
	}
	if !valid(rule) {
		return true
	}
	for _, spelling := range []string{jsTrim(command), jsTrim(collapse(command)), jsTrim(unquote(command))} {
		if matches(rule, spelling) {
			return true
		}
	}
	return false
}

// valid reports whether rule, collapsed and trimmed, is one the desktop
// accepts: not empty, not too long, not only wildcards, and with an
// executable before a colon.
func valid(rule string) bool {
	rule = jsTrim(collapse(rule))
	if rule == "" || len(utf16.Encode([]rune(rule))) > maxRule || wildcards(rule) || strings.HasPrefix(rule, ":") {
		return false
	}
	if executable, args, ok := colonRule(rule); ok && wildcards(executable) && (args == "" || wildcards(args)) {
		return false
	}
	return true
}

// wildcards reports whether s is one or more "*" and nothing else.
func wildcards(s string) bool { return s != "" && strings.Trim(s, "*") == "" }

// colonRule splits a rule of the form "executable:arguments", such as
// "aws:*s3 rm*". A rule whose part before the first colon has whitespace
// inside is not one.
func colonRule(rule string) (executable, args string, ok bool) {
	i := strings.IndexByte(rule, ':')
	if i <= 0 {
		return "", "", false
	}
	executable = jsTrim(rule[:i])
	if strings.ContainsFunc(executable, jsSpace) {
		return "", "", false
	}
	return executable, jsTrim(rule[i+1:]), true
}

// matches reports whether rule matches command: as a colon rule, its
// executable pattern the command's first word and its argument pattern the
// rest; else, with a wildcard, the whole command; else the command or a
// prefix of it that ends at a space.
func matches(rule, command string) bool {
	if executable, args, ok := colonRule(rule); ok {
		first, rest, _ := strings.Cut(command, " ")
		if wildcard(executable, first) && wildcard(args, jsTrim(rest)) {
			return true
		}
	}
	if strings.Contains(rule, "*") {
		return wildcard(rule, command)
	}
	return command == rule || strings.HasPrefix(command, rule+" ")
}

// wildcard reports whether pattern, in which "*" stands for any characters
// of one line, matches all of s: the desktop's regular expression
// ^…$ with .* for every "*", where . matches no line terminator.
func wildcard(pattern, s string) bool {
	parts := strings.Split(jsTrim(pattern), "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	re, err := regexp.Compile(`\A` + strings.Join(parts, `[^\n\r\x{2028}\x{2029}]*`) + `\z`)
	if err != nil {
		return jsTrim(pattern) == s
	}
	return re.MatchString(s)
}

// collapse replaces every run of the whitespace the desktop's Mg knows
// with one space and drops it at both ends.
func collapse(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if collapsed(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// collapsed reports whether r is whitespace to the desktop's Mg.
func collapsed(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r', 0xa0, 0x200b, 0x200c, 0x200d, 0xfeff:
		return true
	}
	return false
}

// escapes are the characters ANSI-C quoting ($'…') writes with a backslash
// and a letter.
var escapes = map[rune]rune{
	'a': '\a', 'b': '\b', 'e': 0x1b, 'E': 0x1b, 'f': '\f', 'n': '\n', 'r': '\r', 't': '\t', 'v': '\v',
	'\\': '\\', '\'': '\'', '"': '"', '?': '?',
}

// The states of unquote: outside quotes, in single quotes, in double quotes
// and in ANSI-C quotes.
const (
	plain = iota
	single
	double
	ansi
)

// unquote removes the quoting of a command the way the desktop's mZt does,
// so that r\m and 'rm' read rm: backslashes, single and double quotes and
// ANSI-C quoting, without expanding anything. The result is collapsed.
func unquote(command string) string {
	in := []rune(command)
	var out []rune
	state := plain
	for i := 0; i < len(in); {
		var n int
		switch state {
		case plain:
			out, state, n = unquotePlain(out, in[i:])
		case single:
			out, state, n = unquoteSingle(out, in[i:])
		case double:
			out, state, n = unquoteDouble(out, in[i:])
		default:
			out, state, n = unquoteANSI(out, in[i:])
		}
		i += n
	}
	return collapse(string(out))
}

// The unquote functions append what in, the rest of the command, starts
// with to out, and return the state after it and how much of in it took.

func unquotePlain(out, in []rune) ([]rune, int, int) {
	c, next := in[0], rune(-1)
	if len(in) > 1 {
		next = in[1]
	}
	switch {
	case c == '$' && next == '\'':
		return out, ansi, 2
	case c == '$' && next == '"':
		return out, double, 2
	case c == '\'':
		return out, single, 1
	case c == '"':
		return out, double, 1
	case c == '\\' && next >= 0:
		if next != '\n' {
			out = append(out, next)
		}
		return out, plain, 2
	}
	return append(out, c), plain, 1
}

func unquoteSingle(out, in []rune) ([]rune, int, int) {
	if in[0] == '\'' {
		return out, plain, 1
	}
	return append(out, in[0]), single, 1
}

func unquoteDouble(out, in []rune) ([]rune, int, int) {
	c := in[0]
	if c == '"' {
		return out, plain, 1
	}
	if c == '\\' && len(in) > 1 && strings.ContainsRune("$`\"\\\n", in[1]) {
		if in[1] != '\n' {
			out = append(out, in[1])
		}
		return out, double, 2
	}
	return append(out, c), double, 1
}

func unquoteANSI(out, in []rune) ([]rune, int, int) {
	c := in[0]
	if c == '\'' {
		return out, plain, 1
	}
	if c != '\\' || len(in) < 2 {
		return append(out, c), ansi, 1
	}
	letter := in[1]
	if r, ok := escapes[letter]; ok {
		return append(out, r), ansi, 2
	}
	if value, n, ok := ansiNumber(in); ok {
		return append(out, value), ansi, n
	}
	return append(out, letter), ansi, 2
}

// ansiNumber reads the character that in, which starts with a backslash,
// gives by its number: \xHH, \uHHHH, \UHHHHHHHH or \OOO. n is how much of
// in the number takes.
func ansiNumber(in []rune) (value rune, n int, ok bool) {
	digits, base, maxDigits, from := "0123456789abcdefABCDEF", 16, 2, 2
	unicode := false
	switch in[1] {
	case 'x':
	case 'u':
		maxDigits, unicode = 4, true
	case 'U':
		maxDigits, unicode = 8, true
	default:
		digits, base, maxDigits, from = "01234567", 8, 3, 1
	}
	number, count := 0, 0
	for count < maxDigits && from+count < len(in) && strings.ContainsRune(digits, in[from+count]) {
		number = number*base + strings.IndexRune("0123456789abcdef", toLower(in[from+count]))
		count++
	}
	if count == 0 || unicode && number > 0x10ffff {
		return 0, 0, false
	}
	return rune(number), from + count, true //nolint:gosec // at most eight hexadecimal digits, checked for Unicode
}

func toLower(r rune) rune {
	if r >= 'A' && r <= 'F' {
		return r + 'a' - 'A'
	}
	return r
}

// jsSpace is a character that JavaScript's trim removes.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }
