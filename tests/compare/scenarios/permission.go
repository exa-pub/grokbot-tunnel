package scenarios

import (
	"fmt"

	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

// decisions is one scenario for every combination of the permission mode,
// who decides and what the backend says about the request: a request of each
// kind, and what is on disk afterwards.
func decisions() []Scenario {
	authorizations := []struct {
		name string
		auth Authorization
	}{
		{"plain", Authorization{}},
		{"standing", Authorization{ByStanding: true}},
		{"approval", Authorization{ByApproval: true}},
		{"approval-id", Authorization{ByApproval: true, ApprovalID: "approval-1"}},
		{"id-only", Authorization{ApprovalID: "approval-1"}},
	}
	modes := []string{"always", "ask", "never"}
	out := make([]Scenario, 0, len(modes)*2*len(authorizations))
	for _, mode := range modes {
		for _, authoritative := range []bool{false, true} {
			for _, a := range authorizations {
				who := "local"
				if authoritative {
					who = "server"
				}
				out = append(out, Scenario{
					ID:         fmt.Sprintf("permission/%s-%s-%s", mode, who, a.name),
					About:      "who may run what: mode " + mode + ", decided by the " + who + ", request marked " + a.name,
					Permission: mode, ServerAuthoritative: authoritative, Patches: ripgrep,
					Files: []File{Text("notes.txt", "secret\n")},
					Steps: []Step{
						Do(With(Shell("r1", "echo ran > ran.txt; echo hello"), a.auth)),
						Do(With(Read("r2", "notes.txt"), a.auth)),
						Do(With(Ls("r3", "."), a.auth)),
						Do(With(Upload("r4", "new.txt", []byte("x")), a.auth)),
						Do(With(Download("r5", "notes.txt"), a.auth)),
						Do(With(Discover("r6", "codex"), a.auth)),
						Do(With(spawn("r7", "echo bg > bg.txt"), a.auth)),
					},
					Watch: []string{"new.txt", "ran.txt", "bg.txt"},
				})
			}
		}
	}
	return out
}

var permission = decisions()
