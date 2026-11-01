package entity

import "fmt"

// Mode is the standing permission mode for local actions.
type Mode string

// Modes the user can choose.
const (
	ModeAsk    Mode = "ask"
	ModeAlways Mode = "always"
	ModeNever  Mode = "never"
)

// ParseMode returns the mode named s.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case ModeAsk, ModeAlways, ModeNever:
		return m, nil
	}
	return "", fmt.Errorf("permission must be ask, always or never, not %q", s)
}
