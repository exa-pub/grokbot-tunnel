package localexec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

// outlives is added to the prompt for a command that can keep running after
// its request, like the desktop's outlivesScope.
const outlives = "; it can keep running after this request ends"

// describe returns a human-readable action for the permission prompt, or ""
// if the exec request is not supported, like the desktop's XDe. Every
// server-supplied string is quoted, so control characters and spaces cannot
// rewrite what the prompt shows.
func describe(msg *agentv1.ExecServerMessage) string {
	switch {
	case msg.GetShellStreamArgs() != nil:
		x := msg.GetShellStreamArgs()
		action := fmt.Sprintf("Run %q in %q", x.Command, x.WorkingDirectory)
		if x.IsBackground {
			action += outlives
		}
		return action
	case msg.GetBackgroundShellSpawnArgs() != nil:
		x := msg.GetBackgroundShellSpawnArgs()
		return fmt.Sprintf("Run background %q in %q", x.Command, x.WorkingDirectory) + outlives
	case msg.GetReadArgs() != nil:
		return fmt.Sprintf("Read %q", msg.GetReadArgs().Path)
	case msg.GetLsArgs() != nil:
		return fmt.Sprintf("List %q", msg.GetLsArgs().Path)
	}
	return ""
}

// execCases are the cases of the desktop's ExecServerMessage, by their
// protobuf field names. Only some of them are in api/proto.
var execCases = []string{
	"shell_args", "write_args", "delete_args", "grep_args", "read_args", "redacted_read_args", "ls_args",
	"diagnostics_args", "request_context_args", "mcp_args", "shell_stream_args", "background_shell_spawn_args",
	"list_mcp_resources_exec_args", "read_mcp_resource_exec_args", "mcp_state_exec_args", "fetch_args",
	"record_screen_args", "computer_use_args", "write_shell_stdin_args", "execute_hook_args", "subagent_args",
	"force_background_shell_args", "force_background_subagent_args", "subagent_await_args",
	"smart_mode_classifier_args", "canvas_diagnostics_args", "shell_allowlist_precheck_args",
	"mcp_allowlist_precheck_args", "web_fetch_allowlist_precheck_args", "git_diff_request",
	"mini_swe_agent_bash_args", "conversation_search_args", "agent_store_conflict_args",
}

// execCaseNames maps the JSON keys of execCases, the protobuf name and the
// JSON name, to the case name the desktop reports, the JSON name.
var execCaseNames = func() map[string]string {
	names := make(map[string]string, 2*len(execCases))
	for _, field := range execCases {
		parts := strings.Split(field, "_")
		for i := 1; i < len(parts); i++ {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
		name := strings.Join(parts, "")
		names[field] = name
		names[name] = name
	}
	return names
}()

// execCase returns the case of the exec server message in serverJSON as the
// desktop names it, such as "writeArgs": its first top-level key that is a
// case of the desktop's message; "" if there is none. Other keys, such as
// id, execId or spanContext, are fields outside the case.
func execCase(serverJSON string) string {
	dec := json.NewDecoder(bytes.NewReader([]byte(serverJSON)))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		key, ok := tok.(string)
		if !ok {
			return ""
		}
		if name, ok := execCaseNames[key]; ok {
			return name
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
}
