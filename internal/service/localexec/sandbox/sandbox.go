// Package sandbox answers for the sandbox a command is asked to run in, like
// the desktop's local-exec daemon on Linux. The desktop sandboxes commands
// through a helper program that its Linux release neither ships nor
// configures, so there a command with a sandbox policy other than none does
// not run: it is refused as unsupported. grokbot-tunnel has no sandbox
// either and refuses the same way, rather than run a command without the
// sandbox that was asked for.
package sandbox

import (
	"fmt"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

// Reason is why the desktop's daemon cannot sandbox on Linux, in its words.
const Reason = "Sandbox binary path was not configured"

// Effective returns the policy a command runs under, as the desktop reports
// it in the start event of a shell command: the requested one, with the
// fields the desktop keeps of it (its Kc and Xk), and none if nothing was
// requested. Only a command whose approval is skipped and that requests
// nothing has no policy at all.
func Effective(requested *agentv1.SandboxPolicy, skipApproval bool) *agentv1.SandboxPolicy {
	if requested == nil {
		if skipApproval {
			return nil
		}
		return &agentv1.SandboxPolicy{Type: agentv1.SandboxPolicy_TYPE_INSECURE_NONE}
	}
	if !workspace(requested.Type) {
		// No sandbox, also for a type this version does not know.
		p := &agentv1.SandboxPolicy{Type: agentv1.SandboxPolicy_TYPE_INSECURE_NONE}
		if requested.Type <= agentv1.SandboxPolicy_TYPE_INSECURE_NONE {
			p.AllowlistEscalated = requested.AllowlistEscalated
			p.EnableSharedBuildCache = requested.EnableSharedBuildCache
			p.DebugOutputDir = requested.DebugOutputDir
			p.CaptureDenies = requested.CaptureDenies
		}
		return p
	}
	network := networkPolicy(requested)
	access := network.GetDefaultAction() == agentv1.NetworkPolicy_DEFAULT_ACTION_ALLOW || len(network.Allow) > 0
	p := &agentv1.SandboxPolicy{
		Type:                    requested.Type,
		NetworkAccess:           &access,
		AdditionalReadonlyPaths: requested.AdditionalReadonlyPaths,
		DebugOutputDir:          requested.DebugOutputDir,
		DisableTmpWrite:         requested.DisableTmpWrite,
		EnableSharedBuildCache:  requested.EnableSharedBuildCache,
		NetworkPolicy:           network,
		NetworkPolicyStrict:     requested.NetworkPolicyStrict,
		CaptureDenies:           requested.CaptureDenies,
		SkipStatsigDefaults:     requested.SkipStatsigDefaults,
	}
	if requested.Type == agentv1.SandboxPolicy_TYPE_WORKSPACE_READWRITE {
		p.AdditionalReadwritePaths = requested.AdditionalReadwritePaths
	}
	switch requested.ReadBoundary {
	case agentv1.SandboxPolicy_READ_BOUNDARY_MODE_WORKSPACE:
		p.ReadBoundary = requested.ReadBoundary
		p.AdditionalReadPaths = requested.AdditionalReadPaths
	case agentv1.SandboxPolicy_READ_BOUNDARY_MODE_SYSTEM:
		p.ReadBoundary = requested.ReadBoundary
	case agentv1.SandboxPolicy_READ_BOUNDARY_MODE_UNSPECIFIED:
	}
	return p
}

// Unsupported returns the refusal of command, which was to run in cwd under
// policy p (Effective), or nil if p asks for no sandbox.
func Unsupported(p *agentv1.SandboxPolicy, command, cwd string) *agentv1.ShellSandboxUnsupported {
	if !workspace(p.GetType()) {
		return nil
	}
	return &agentv1.ShellSandboxUnsupported{
		Command:           command,
		WorkingDirectory:  cwd,
		SandboxPolicyType: typeName(p.Type),
		Reason:            Reason,
		IsReadonly:        p.Type == agentv1.SandboxPolicy_TYPE_WORKSPACE_READONLY,
	}
}

// Error is the desktop's error for a command refused with u, as the
// terminal file of a background command has it.
func Error(u *agentv1.ShellSandboxUnsupported) string {
	return fmt.Sprintf("Sandbox policy '%s' is not supported on this system. Ensure the sandbox helper binary is available, "+
		"or use 'insecure_none'. Reason: %s", u.SandboxPolicyType, u.Reason)
}

// workspace reports whether t asks for a sandbox.
func workspace(t agentv1.SandboxPolicy_Type) bool {
	return t == agentv1.SandboxPolicy_TYPE_WORKSPACE_READWRITE || t == agentv1.SandboxPolicy_TYPE_WORKSPACE_READONLY
}

func typeName(t agentv1.SandboxPolicy_Type) string {
	if t == agentv1.SandboxPolicy_TYPE_WORKSPACE_READONLY {
		return "workspace_readonly"
	}
	return "workspace_readwrite"
}

// networkPolicy is the network policy of a requested workspace sandbox as
// the desktop keeps it: the given one with a version only if it is 1, an
// action only if it allows or denies, and a log format only if it is jsonl;
// without one, version 1 of allow or deny by the policy's networkAccess.
func networkPolicy(requested *agentv1.SandboxPolicy) *agentv1.NetworkPolicy {
	in := requested.NetworkPolicy
	if in == nil {
		version := uint32(1)
		action := agentv1.NetworkPolicy_DEFAULT_ACTION_DENY
		if requested.GetNetworkAccess() {
			action = agentv1.NetworkPolicy_DEFAULT_ACTION_ALLOW
		}
		return &agentv1.NetworkPolicy{Version: &version, DefaultAction: &action}
	}
	out := &agentv1.NetworkPolicy{Deny: in.Deny, Allow: in.Allow}
	if in.GetVersion() == 1 {
		out.Version = in.Version
	}
	if action := in.GetDefaultAction(); action == agentv1.NetworkPolicy_DEFAULT_ACTION_ALLOW ||
		action == agentv1.NetworkPolicy_DEFAULT_ACTION_DENY {
		out.DefaultAction = &action
	}
	if in.Logging != nil {
		out.Logging = &agentv1.NetworkPolicyLoggingConfig{}
		if in.Logging.GetDecisionLogPath() != "" {
			out.Logging.DecisionLogPath = in.Logging.DecisionLogPath
		}
		if in.Logging.GetLogFormat() == "jsonl" {
			out.Logging.LogFormat = in.Logging.LogFormat
		}
	}
	return out
}
