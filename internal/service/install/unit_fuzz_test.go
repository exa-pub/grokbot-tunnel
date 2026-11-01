package install

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzQuoteExec checks that systemd, as modeled by splitUnit, reads the
// arguments back exactly as given.
func FuzzQuoteExec(f *testing.F) {
	f.Add("/usr/bin/grokbot-tunnel", "--root=/home/a b", "100%$HOME")
	f.Add("", `"quoted" 'single' \back;`, "\t")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		args := []string{a, b, c}
		line, err := quoteExec(args)
		if err != nil {
			return
		}
		got, err := splitUnit(line, true)
		if err != nil || !slices.Equal(got, args) {
			t.Fatalf("%q -> %q -> %q, %v", args, line, got, err)
		}
	})
}

// FuzzQuoteEnv checks that an Environment= entry reads back as one word.
func FuzzQuoteEnv(f *testing.F) {
	f.Add("HOME=/home/a b")
	f.Add(`PS1=\u "%d"`)
	f.Add("=x")
	f.Fuzz(func(t *testing.T, kv string) {
		line, err := quoteEnv(kv)
		if err != nil {
			return
		}
		got, err := splitUnit(line, false)
		if err != nil || len(got) != 1 || got[0] != kv {
			t.Fatalf("%q -> %q -> %q, %v", kv, line, got, err)
		}
	})
}

var errSyntax = errors.New("not valid in a unit file")

// splitUnit models how systemd reads a setting: specifiers (%%), then words
// split on whitespace, with double or single quotes and C escapes, and with
// vars set, $$ as a literal $. It rejects anything quoteExec and quoteEnv must
// never produce, such as a lone % or an escape other than \\ and \".
func splitUnit(line string, vars bool) ([]string, error) {
	if !utf8.ValidString(line) {
		return nil, errSyntax
	}
	var words []string
	for i := 0; i < len(line); {
		if line[i] == ' ' || line[i] == '\t' {
			i++
			continue
		}
		var w strings.Builder
		quote := byte(0)
		if line[i] == '"' || line[i] == '\'' {
			quote = line[i]
			i++
		}
		for ; ; i++ {
			if i == len(line) {
				if quote != 0 {
					return nil, errSyntax
				}
				break
			}
			ch := line[i]
			if quote == 0 && (ch == ' ' || ch == '\t') {
				break
			}
			if quote != 0 && ch == quote {
				i++
				if i < len(line) && line[i] != ' ' && line[i] != '\t' {
					return nil, errSyntax
				}
				break
			}
			switch {
			case quote == 0 && (ch == '"' || ch == '\'' || ch == '\\' || ch == ';'):
				return nil, errSyntax
			case ch == '\\':
				if i+1 == len(line) || line[i+1] != '\\' && line[i+1] != '"' {
					return nil, errSyntax
				}
				i++
				w.WriteByte(line[i])
			case ch == '%' || vars && ch == '$':
				if i+1 == len(line) || line[i+1] != ch {
					return nil, errSyntax
				}
				i++
				w.WriteByte(ch)
			default:
				w.WriteByte(ch)
			}
		}
		words = append(words, w.String())
	}
	return words, nil
}
