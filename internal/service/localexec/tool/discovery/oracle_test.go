//go:build compare

package discovery

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// titlePieces are what the random texts are made of: keys and tokens the
// desktop knows and others, with what may stand before and after them,
// links, emails, paths, long words, whitespace and characters next to
// which a pattern stops.
var titlePieces = []string{
	"sk-", "pk-", "rk-", "sk_live_", "ghp_", "github_pat_", "gho_", "xoxb-", "xoxp-", "AKIA", "AIza", "glpat-", "hf_", "npm_",
	"Bearer ", "bearer  ", "BEARER\t", "Authorization: Basic ", "eyJ", "token=", "key: ", "secret = ", `password="`, "a=b:", "==", "=", ":",
	"0123456789abcdef", "DEADBEEF", "ABCDEFGHIJKLMNOP", "abcdefghijklmnopqrstuvwxyz", "Zm9vYmFyYmF6cXV4", "a1B2-c3_D4", "wJalrXUtnFEMI/K7MDENG",
	"0123456789ABCDEF0123456789ABCDEF", "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN", "AbCdEfGhIjKlMnOpQrStUvWxYz012345+/_-",
	"https://", "HTTP://", "ftp://", "git+ssh://", "s://", "example.com/a/b?c=d", "user@", "@example.com", "me@host.io", ".co", ".x",
	"/usr/local/bin", "/home/me/.ssh/id_rsa", "/a", "/a/b", "~/x/y", "./a/b", "../a/b", ".../a/b", "src/main/java", "//", "/",
	`C:\Users\me`, `d:\a\b\`, `\\host\share`, `\`, "~", ".", "..", "-", "_", "+", "%",
	" ", "  ", "\n", "\r\n", "\t", "\u00a0", "\u2028", "\ufeff", "\u200b", "\u3000",
	"fix the bug in ", "run ", " and ", "x", "Z", "9", "é", "ж", "日本", "🙂", "ſ", "K", "İ",
	`"`, "'", "`", "<", ">", ",", ";", ")", "(", "[", "]", "{", "}", "!", "#", "&", "*", "|",
}

func randomTitle(r *rand.Rand) string {
	var b strings.Builder
	for range 1 + r.IntN(14) {
		piece := titlePieces[r.IntN(len(titlePieces))]
		b.WriteString(piece)
		if r.IntN(6) == 0 {
			// Runs long enough for the patterns that count characters.
			b.WriteString(strings.Repeat(piece, r.IntN(6)))
		}
	}
	return b.String()
}

// A title that the desktop cuts inside a surrogate pair ends with the
// pair's first half; grokbot-tunnel leaves the half out, since it does not
// send text that is not Unicode. The oracle's answer arrives with U+FFFD for
// the half.
func wholePairs(desktop string) string {
	if cut, ok := strings.CutSuffix(desktop, "\ufffd…"); ok {
		return strings.TrimRightFunc(cut, jsSpace) + "…"
	}
	return desktop
}

// The desktop's part of the redaction gives the desktop's titles, on random
// texts: with the desktop's limit and, to compare all of a text, without.
// What grokbot-tunnel redacts besides is applied to that result, so its
// titles show no more than the desktop's.
func TestOracleDiscoveryTitle(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(5, 6))
	for range *compattest.OracleRuns {
		text := randomTitle(r)
		var limited, whole string
		oracle.Call(&limited, "discoveryTitle", text)
		oracle.Call(&whole, "discoveryTitle", text, nil)
		if got := shorten(redactDesktop(text)); got != wholePairs(limited) {
			t.Errorf("title of %q\n got     %q\n desktop %q", text, got, limited)
		}
		if got := redactDesktop(text); got != whole {
			t.Errorf("redaction of %q\n got     %q\n desktop %q", text, got, whole)
		}
	}
}

// The title of a command, its parts redacted one by one.
func TestOracleCommandTitle(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(7, 8))
	for range *compattest.OracleRuns {
		parts := make([]string, 1+r.IntN(4))
		for i := range parts {
			parts[i] = randomTitle(r)
		}
		var want string
		oracle.Call(&want, "commandTitle", parts)
		if got := joinTitle(parts, redactDesktop); got != wholePairs(want) {
			t.Errorf("title of %q\n got     %q\n desktop %q", parts, got, want)
		}
	}
}
