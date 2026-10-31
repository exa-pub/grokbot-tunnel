// Package permission decides whether a local action may run, asking the user
// when the standing mode says so.
package permission

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// Modes accepted by Policy.Mode.
const (
	ModeAsk    = "ask"
	ModeAlways = "always"
	ModeNever  = "never"
)

// The error texts are shown to the agent as complete sentences explaining what
// the user can do, hence the capitalization and punctuation.
//
//nolint:revive,staticcheck // user-facing sentences
var (
	ErrRefused = errors.New("That action was not approved on the user's computer, so nothing ran. " +
		"Ask the user to approve it (or to set Settings → Bot → Execution on Local Computer to \"Always allow\"), " +
		"and use your own computer (Shell, Read, AwaitShell) in the meantime.")
	ErrDisabled = errors.New("Local tools are turned off. " +
		"The user has set local tool access to \"Never\", so Shell, Read, and AwaitShell with machineId, " +
		"plus CopyToBox and CopyFromBox, cannot run on their computer. " +
		"Do not retry them while this setting remains \"Never\". " +
		"Use your own computer instead by omitting machineId, " +
		"or ask the user to change the setting in Settings → Bot → Execution on Local Computer. " +
		"If they change it away from \"Never\", you may try again.")
)

// Prompter asks the user a question; adapter/tty implements it.
type Prompter interface {
	// Interactive reports whether a question can be asked now.
	Interactive() bool
	// Ask shows question and returns the answer; an empty answer refuses.
	Ask(ctx context.Context, question string) (string, error)
}

// Policy holds the standing permission mode and the terminal used for
// prompts, like the desktop's local-use permission check (isLocalUseBlocked).
type Policy struct {
	mu       sync.Mutex    // guards Mode and prompt
	prompt   chan struct{} // one-slot semaphore serializing terminal prompts
	Mode     string
	Prompter Prompter // nil: never ask
}

// Standing returns the current permission mode.
func (p *Policy) Standing() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Mode
}

func (p *Policy) setStanding(mode string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Mode = mode
}

// decide resolves the request without asking the user. ok is false when the
// user has to be prompted.
func (p *Policy) decide(server, approved bool) (ok bool, err error) {
	switch {
	case p.Standing() == ModeNever:
		return true, ErrDisabled
	case server && approved:
		return true, nil
	case server:
		return true, ErrRefused
	case p.Standing() == ModeAlways:
		return true, nil
	}
	return false, nil
}

// Allow returns nil if action may run. When server is set, the server's
// approval flag is authoritative and the user is never prompted.
func (p *Policy) Allow(ctx context.Context, action string, server, approved bool) error {
	if ok, err := p.decide(server, approved); ok {
		return err
	}
	if !p.interactive() {
		return ErrRefused
	}
	// Wait for the terminal without ignoring cancellation, so a queued request
	// canceled by the server does not hold its worker slot.
	slot := p.promptSlot()
	select {
	case slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-slot }()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Another prompt may have switched the mode to "always" while we waited.
	if ok, err := p.decide(server, approved); ok {
		return err
	}
	if !p.interactive() {
		return ErrRefused
	}
	answer, err := p.Prompter.Ask(ctx, action+"? [y/N/a] ")
	if err != nil {
		return err
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "a":
		p.setStanding(ModeAlways)
		return nil
	case "y", "yes":
		return nil
	}
	return ErrRefused
}

func (p *Policy) promptSlot() chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prompt == nil {
		p.prompt = make(chan struct{}, 1)
	}
	return p.prompt
}

func (p *Policy) interactive() bool { return p.Prompter != nil && p.Prompter.Interactive() }
