// Package denylist refuses the commands an administrator's denylist blocks,
// like the desktop's local-exec daemon: the server sends the list with a
// command, and a command that one of its rules matches is not run.
//
// A rule matches a command by its beginning ("rm" matches "rm -f x"), with
// "*" as a wildcard, or as "executable:arguments" patterns. It is tried on
// the whole command and on every simple command in it, also those in
// pipelines, lists, subshells and substitutions.
//
// The desktop finds the simple commands with the bash grammar of
// tree-sitter; grokbot-tunnel with a bash parser in Go, corrected where the
// two read a command differently (commands.go). What that parser does not
// accept, tree-sitter may: such a command is refused as one that cannot be
// analyzed, which is what the desktop says of a command it cannot parse. So
// grokbot-tunnel may refuse a command the desktop would run, never the other
// way around, as far as the oracle test can tell.
package denylist

import "slices"

// Unanalyzable is the desktop's reason for a command it cannot check
// against the denylist.
const Unanalyzable = "Denied: this command could not be conclusively analyzed against your team's administrator command " +
	"denylist, so it was blocked (fail-closed) and was not executed. It cannot be approved from this conversation; only a " +
	"user can run it manually outside the agent. You may continue working on the task."

// blockedBy is the desktop's reason for a command that rule blocks.
func blockedBy(rule string) string {
	return "Denied: this command was blocked by administrator policy (denylist rule: " + rule + ") and was not executed. " +
		"It cannot be approved from this conversation; only a user can run it manually outside the agent. " +
		"You may continue working on the task."
}

// Refusal returns the reason the denylist rules give for not running
// command, in the desktop's words, or "" if it may run. Without rules every
// command may; with rules, a command in which no simple command is found
// may not, whatever the rules are.
func Refusal(command string, rules []string) string {
	if len(rules) == 0 {
		return ""
	}
	found, ok := commands(command)
	if !ok || len(found) == 0 {
		return Unanalyzable
	}
	forms := []string{command}
	for _, c := range found {
		if !slices.Contains(forms, c) {
			forms = append(forms, c)
		}
	}
	if rule, blocked := blockingRule(forms, rules); blocked {
		return blockedBy(rule)
	}
	return ""
}
