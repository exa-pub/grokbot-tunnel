// Package scenarios lists the comparison scenarios, a file for each group.
package scenarios

import (
	"fmt"

	"github.com/exa-pub/grokbot-tunnel/tests/compare/scenario"
)

// All returns every scenario; IDs are unique.
func All() []scenario.Scenario {
	var all []scenario.Scenario
	for _, group := range [][]scenario.Scenario{shell, background, read, ls, transfer, root, discovery, permission, dispatch, channel} {
		all = append(all, group...)
	}
	seen := map[string]bool{}
	for _, s := range all {
		if s.ID == "" || seen[s.ID] {
			panic(fmt.Sprintf("scenarios: ID %q is empty or used twice", s.ID))
		}
		seen[s.ID] = true
	}
	return all
}
