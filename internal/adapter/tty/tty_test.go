package tty_test

import (
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/tty"
)

func TestPromptEscapesControlCharacters(t *testing.T) {
	for in, want := range map[string]string{
		"Run x":                    "Run x",
		"Read /a\r\x1b[2KRead /b":  `Read /a\r\x1b[2KRead /b`,
		"List \u202egpj.exe":       `List \u202egpj.exe`,
		"Run \"ls\" in /tmp/\nyes": `Run "ls" in /tmp/\nyes`,
		"Read /домашний":           "Read /домашний",
	} {
		if got := tty.Printable(in); got != want {
			t.Errorf("Printable(%q) = %q, want %q", in, got, want)
		}
	}
}
