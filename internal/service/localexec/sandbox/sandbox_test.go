package sandbox_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/sandbox"
)

func policy(t *testing.T, json string) *agentv1.SandboxPolicy {
	t.Helper()
	p := &agentv1.SandboxPolicy{}
	require.NoError(t, protojson.Unmarshal([]byte(json), p))
	return p
}

// The policy of the start event, as the desktop 1.0.1 answered in the
// comparison scenarios shell/sandbox-*.
func TestEffective(t *testing.T) {
	for _, tc := range []struct {
		name, requested string
		skip            bool
		want            string // "" is no policy
	}{
		{"nothing requested", "", false, `{"type":"TYPE_INSECURE_NONE"}`},
		{"nothing requested, approval skipped", "", true, ""},
		{"no type", `{}`, true, `{"type":"TYPE_INSECURE_NONE"}`},
		{"unknown type", `{"type":9,"captureDenies":true}`, false, `{"type":"TYPE_INSECURE_NONE"}`},
		{
			"none keeps four fields",
			`{"type":"TYPE_INSECURE_NONE","networkAccess":true,"allowlistEscalated":true,"enableSharedBuildCache":true,
			  "debugOutputDir":"/d","captureDenies":true,"additionalReadonlyPaths":["/ro"]}`, false,
			`{"type":"TYPE_INSECURE_NONE","allowlistEscalated":true,"enableSharedBuildCache":true,"debugOutputDir":"/d","captureDenies":true}`,
		},
		{
			"read-only, network denied by default", `{"type":"TYPE_WORKSPACE_READONLY","networkAccess":false}`, true,
			`{"type":"TYPE_WORKSPACE_READONLY","networkAccess":false,"networkPolicy":{"version":1,"defaultAction":"DEFAULT_ACTION_DENY"}}`,
		},
		{
			"read-write with everything",
			`{"type":"TYPE_WORKSPACE_READWRITE","networkAccess":true,"additionalReadwritePaths":["/rw"],"additionalReadonlyPaths":["/ro"],
			  "debugOutputDir":"/d","disableTmpWrite":true,"allowlistEscalated":true,"enableSharedBuildCache":false,
			  "networkPolicyStrict":false,"captureDenies":true,"skipStatsigDefaults":true,"readBoundary":"READ_BOUNDARY_MODE_WORKSPACE",
			  "additionalReadPaths":["/read"],"allowUserOverrideReadPaths":true}`, false,
			`{"type":"TYPE_WORKSPACE_READWRITE","networkAccess":true,"additionalReadwritePaths":["/rw"],"additionalReadonlyPaths":["/ro"],
			  "debugOutputDir":"/d","disableTmpWrite":true,"enableSharedBuildCache":false,"networkPolicyStrict":false,"captureDenies":true,
			  "skipStatsigDefaults":true,"readBoundary":"READ_BOUNDARY_MODE_WORKSPACE","additionalReadPaths":["/read"],
			  "networkPolicy":{"version":1,"defaultAction":"DEFAULT_ACTION_ALLOW"}}`,
		},
		{
			"read-only drops what it cannot use",
			`{"type":"TYPE_WORKSPACE_READONLY","networkAccess":true,"additionalReadwritePaths":["/rw"],"additionalReadonlyPaths":["/ro"],
			  "readBoundary":"READ_BOUNDARY_MODE_SYSTEM","additionalReadPaths":["/read"],"networkPolicy":{"version":2,
			  "defaultAction":"DEFAULT_ACTION_DENY","deny":["bad"],"allow":["good"],"logging":{"decisionLogPath":"/log","logFormat":"text"}}}`,
			false,
			`{"type":"TYPE_WORKSPACE_READONLY","networkAccess":true,"additionalReadonlyPaths":["/ro"],"readBoundary":"READ_BOUNDARY_MODE_SYSTEM",
			  "networkPolicy":{"defaultAction":"DEFAULT_ACTION_DENY","deny":["bad"],"allow":["good"],"logging":{"decisionLogPath":"/log"}}}`,
		},
		{
			"an empty network policy is kept", `{"type":"TYPE_WORKSPACE_READONLY","networkAccess":true,"networkPolicy":{}}`, false,
			`{"type":"TYPE_WORKSPACE_READONLY","networkAccess":false,"networkPolicy":{}}`,
		},
		{
			"jsonl logging", `{"type":"TYPE_WORKSPACE_READONLY","networkPolicy":{"version":1,"defaultAction":"DEFAULT_ACTION_ALLOW",
			  "logging":{"logFormat":"jsonl"}}}`, false,
			`{"type":"TYPE_WORKSPACE_READONLY","networkAccess":true,"networkPolicy":{"version":1,"defaultAction":"DEFAULT_ACTION_ALLOW",
			  "logging":{"logFormat":"jsonl"}}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requested *agentv1.SandboxPolicy
			if tc.requested != "" {
				requested = policy(t, tc.requested)
			}
			got := sandbox.Effective(requested, tc.skip)
			if tc.want == "" {
				assert.Nil(t, got)
				return
			}
			gotJSON, err := protojson.Marshal(got)
			require.NoError(t, err)
			wantJSON, err := protojson.Marshal(policy(t, tc.want))
			require.NoError(t, err)
			assert.JSONEq(t, string(wantJSON), string(gotJSON))
		})
	}
}

// A command that is to run in a sandbox is refused in the desktop's words;
// one that is to run without is not.
func TestUnsupported(t *testing.T) {
	for _, none := range []*agentv1.SandboxPolicy{nil, {}, {Type: agentv1.SandboxPolicy_TYPE_INSECURE_NONE}, {Type: 9}} {
		assert.Nil(t, sandbox.Unsupported(sandbox.Effective(none, false), "cmd", "/w"))
	}
	readonly := sandbox.Unsupported(&agentv1.SandboxPolicy{Type: agentv1.SandboxPolicy_TYPE_WORKSPACE_READONLY}, "cmd", "/w")
	require.NotNil(t, readonly)
	assert.Equal(t, "cmd", readonly.Command)
	assert.Equal(t, "/w", readonly.WorkingDirectory)
	assert.Equal(t, "workspace_readonly", readonly.SandboxPolicyType)
	assert.Equal(t, "Sandbox binary path was not configured", readonly.Reason)
	assert.True(t, readonly.IsReadonly)
	assert.Equal(t, "Sandbox policy 'workspace_readonly' is not supported on this system. Ensure the sandbox helper binary is "+
		"available, or use 'insecure_none'. Reason: Sandbox binary path was not configured", sandbox.Error(readonly))

	readwrite := sandbox.Unsupported(&agentv1.SandboxPolicy{Type: agentv1.SandboxPolicy_TYPE_WORKSPACE_READWRITE}, "cmd", "/w")
	require.NotNil(t, readwrite)
	assert.Equal(t, "workspace_readwrite", readwrite.SandboxPolicyType)
	assert.False(t, readwrite.IsReadonly)
}
