package agent

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestBuildCodexArgsPinsRestrictedMCPAfterCustomArgs(t *testing.T) {
	args := buildCodexArgs(ExecOptions{
		McpConfig:          json.RawMessage(`{"mcpServers":{"shared":{"url":"http://127.0.0.1:34567/mcp","headers":{"X-Multica-Mcp-Token":"private"}}}}`),
		ExtraArgs:          []string{"-c", `mcp_servers.shared.url="http://machine.example"`},
		CustomArgs:         []string{"--config", `mcp_servers.shared.command="other"`, "-c", `model="other-model"`},
		CodexPinnedMCPURLs: map[string]string{"shared": "http://127.0.0.1:34567/mcp"},
	}, slog.Default())
	wantLast := []string{"-c", `mcp_servers.shared.url="http://127.0.0.1:34567/mcp"`}
	if !slices.Equal(args[len(args)-2:], wantLast) {
		t.Fatalf("restricted MCP URL is not pinned in the final args: %v", args)
	}
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"machine.example", "mcp_servers.shared.command", "private"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unexpected value %q in Codex args", forbidden)
		}
	}
}
