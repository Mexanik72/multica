package protocol

import (
	"encoding/json"
	"regexp"
)

const DaemonCapabilityWorkspaceMCPToolsV1 = "workspace-mcp-tools-v1"

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
