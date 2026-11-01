package entity

import (
	"runtime"
	"strings"
	"unicode"
)

// Machine identifies this computer to the server.
type Machine struct {
	MachineID string `json:"machine_id"`
	// Label is the computer's name in Grok Bot: the one chosen with --label,
	// else DefaultLabel of the current host name. Only a chosen one is saved.
	Label string `json:"label"`
	// HostID is the HostID of the host that created MachineID. A different
	// value means the store was copied from another host.
	HostID string `json:"host_id,omitempty"`
}

// maxLabel is the longest label in code points, as in the desktop.
const maxLabel = 100

// NormalizeLabel trims a label and cuts it to maxLabel code points, like the
// desktop does with the host name. Invalid UTF-8, which the server would
// refuse, becomes U+FFFD.
func NormalizeLabel(label string) string {
	label = TrimSpace(strings.ToValidUTF8(label, "\uFFFD"))
	n := 0
	for i := range label {
		if n == maxLabel {
			return label[:i]
		}
		n++
	}
	return label
}

// DefaultLabel is the label the desktop registers with: the normalized host
// name, or the platform if it is empty.
func DefaultLabel(hostname string) string {
	if label := NormalizeLabel(hostname); label != "" {
		return label
	}
	return runtime.GOOS
}

// TrimSpace removes what JavaScript's String.prototype.trim removes: white
// space, line terminators and the byte order mark, but not U+0085.
func TrimSpace(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		return r == '\uFEFF' || r != '\u0085' && unicode.IsSpace(r)
	})
}
