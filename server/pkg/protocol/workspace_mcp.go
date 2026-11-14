package protocol

import (
	"encoding/json"
	"regexp"
)

const (
	DaemonCapabilityWorkspaceMCPToolsV1       = "workspace-mcp-tools-v1"
	DaemonCapabilityHermesWorkspaceMCPToolsV1 = "hermes-workspace-mcp-tools-v1"
)

func WorkspaceMCPToolsCapability(provider string) string {
	switch provider {
	case "codex":
		return DaemonCapabilityWorkspaceMCPToolsV1
	case "hermes":
		return DaemonCapabilityHermesWorkspaceMCPToolsV1
	default:
		return ""
	}
}

const MaxWorkspaceMCPTools = 1000

var WorkspaceMCPToolName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// RestrictedWorkspaceMCP is carried only in daemon claims. An empty Tools
// array still installs a broker and denies all calls.
type RestrictedWorkspaceMCP struct {
	Name   string          `json:"name"`
	Config json.RawMessage `json:"config"`
	Tools  []string        `json:"tools"`
}

type DaemonHeartbeatPendingMcpProbe struct {
	ID string `json:"id"`
}

const (
	McpProbeCodeTimeout              = "timeout"
	McpProbeCodeUnauthorized         = "unauthorized"
	McpProbeCodeUnsupportedTransport = "unsupported_transport"
	McpProbeCodeHandshake            = "handshake"
	McpProbeCodeInternal             = "internal"
)

// WorkspaceMCPToolList contains tool names and the subset explicitly annotated read-only.
type WorkspaceMCPToolList struct {
	Tools         []string `json:"tools"`
	ReadOnlyTools []string `json:"read_only_tools,omitempty"`
}
