package handler

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func TestAgentMcpToolAccessIsScopedToAssignment(t *testing.T) {
	server := createWorkspaceMcpServerForTest(t, "assignment-policy", workspaceMcpTestEntry)
	researcher := createHandlerTestAgent(t, "researcher", nil)
	triage := createHandlerTestAgent(t, "triage", nil)
	for _, agent := range []string{researcher, triage} {
		addAgentMcpServerForTest(t, agent, server)
	}
	request := func(agent string, tools []string) *http.Request {
		return withURLParams(newRequest(http.MethodPut, "/", map[string]any{
			"mode": "selected", "tools": tools,
		}), "id", agent, "serverId", server)
	}
	readTools := func(agent string) []string {
		req := withURLParam(newRequest(http.MethodGet, "/", nil), "id", agent)
		var result []struct {
			AllowedTools []string `json:"allowed_tools"`
		}
		response := testutil.Call(t, testHandler.ListAgentMcpServers, req).Want(http.StatusOK)
		response.JSON(&result)
		if len(result) != 1 || strings.Contains(response.Body.String(), workspaceMcpTestSecret) {
			t.Fatalf("unexpected assignment response: %s", response.Body.String())
		}
		return result[0].AllowedTools
	}

	testutil.Call(t, testHandler.SetAgentMcpToolAccess, request(researcher, []string{"search_issues"})).Want(http.StatusOK)
	if got := readTools(triage); got != nil {
		t.Fatalf("the other assignment should retain all tools, got %v", got)
	}
	testutil.Call(t, testHandler.SetAgentMcpToolAccess, request(triage, []string{})).Want(http.StatusOK)
	if got := readTools(triage); got == nil || len(got) != 0 {
		t.Fatalf("empty selected list must survive a round trip, got %#v", got)
	}
	taskID := insertHandlerTestTask(t, researcher)
	denied := request(researcher, []string{"create_issue"})
	denied.Header.Set("X-Actor-Source", "task_token")
	denied.Header.Set("X-Agent-ID", researcher)
	denied.Header.Set("X-Task-ID", taskID)
	testutil.Call(t, testHandler.SetAgentMcpToolAccess, denied).Want(http.StatusForbidden)
	if got := readTools(researcher); !slices.Equal(got, []string{"search_issues"}) {
		t.Fatalf("researcher policy changed to %v", got)
	}
}

func TestClaimRestrictedWorkspaceMCP(t *testing.T) {
	for _, tc := range []struct {
		name       string
		provider   string
		capability string
		ownConfig  string
		reject     bool
		message    string
	}{
		{name: "capable Codex daemon", provider: "codex", capability: protocol.DaemonCapabilityWorkspaceMCPToolsV1},
		{name: "old Codex daemon", provider: "codex", reject: true, message: "update the Multica daemon"},
		{name: "capable Hermes daemon", provider: "hermes", capability: protocol.DaemonCapabilityHermesWorkspaceMCPToolsV1},
		{name: "Codex-only daemon for Hermes", provider: "hermes", capability: protocol.DaemonCapabilityWorkspaceMCPToolsV1, reject: true, message: "update the Multica daemon"},
		{name: "agent name collision", provider: "hermes", capability: protocol.DaemonCapabilityHermesWorkspaceMCPToolsV1, ownConfig: `{"mcp":{"shared":{"command":"other"}}}`, reject: true, message: "conflicts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := createWorkspaceMcpServerForTest(t, "shared", workspaceMcpTestEntry)
			runtime := dbfx.Runtime(t, tc.provider, testutil.Cols{
				"runtime_mode": "local", "provider": tc.provider,
			})
			agent := dbfx.Agent(t, "restricted", runtime, testutil.Cols{"runtime_mode": "local"})
			if tc.ownConfig != "" {
				dbfx.Exec(t, "UPDATE agent SET mcp_config = $1 WHERE id = $2", json.RawMessage(tc.ownConfig), agent)
			}
			issue := dbfx.Issue(t, "restricted MCP claim")
			taskID := dbfx.Task(t, agent, testutil.Cols{"runtime_id": runtime, "issue_id": issue})
			addAgentMcpServerForTest(t, agent, server)
			update := withURLParams(newRequest(http.MethodPut, "/", map[string]any{
				"mode": "selected", "tools": []string{},
			}), "id", agent, "serverId", server)
			testutil.Call(t, testHandler.SetAgentMcpToolAccess, update).Want(http.StatusOK)

			req := withURLParam(newOwnerDaemonRequest(http.MethodPost, "/", nil), "runtimeId", runtime)
			req.Header.Set("X-Client-Capabilities", tc.capability)
			resp := testutil.Call(t, testHandler.ClaimTaskByRuntime, req).Want(http.StatusOK)
			var claim struct {
				Task *AgentTaskResponse
			}
			resp.JSON(&claim)
			if tc.reject {
				var status, message string
				dbfx.QueryRow(t, "SELECT status, error FROM agent_task_queue WHERE id = $1", taskID).Scan(&status, &message)
				if claim.Task != nil || status != "failed" || !strings.Contains(message, tc.message) ||
					strings.Contains(message, workspaceMcpTestSecret) {
					t.Fatalf("unexpected rejection: task issued=%v status=%s error=%q", claim.Task != nil, status, message)
				}
				return
			}
			if claim.Task == nil || claim.Task.Agent == nil {
				t.Fatal("expected a claimed task")
			}
			payload := claim.Task.Agent
			if strings.Contains(string(payload.McpConfig), "shared") || len(payload.RestrictedWorkspaceMCP) != 1 ||
				payload.RestrictedWorkspaceMCP[0].Tools == nil || len(payload.RestrictedWorkspaceMCP[0].Tools) != 0 {
				t.Fatal("expected an empty allowlist on the protected daemon field")
			}
		})
	}
}

func TestAssignedWorkspaceMCPProbeKeepsConfigOnDaemonPath(t *testing.T) {
	serverID := createWorkspaceMcpServerForTest(t, "probe-policy", workspaceMcpTestEntry)
	runtimeID := dbfx.Runtime(t, "probe-hermes", testutil.Cols{
		"runtime_mode": "local", "provider": "hermes", "daemon_id": "probe-daemon",
		"metadata": testutil.Raw(`'{"capabilities":["hermes-workspace-mcp-tools-v1"]}'::jsonb`),
	})
	agentID := dbfx.Agent(t, "probe-agent", runtimeID)
	addAgentMcpServerForTest(t, agentID, serverID)
	otherRuntimeID := dbfx.Runtime(t, "other-compatible-runtime", testutil.Cols{
		"runtime_mode": "local", "provider": "hermes",
	})
	// A stale client-supplied runtime cannot override the saved binding.
	start := withURLParams(newRequest(http.MethodPost, "/", map[string]string{"runtime_id": otherRuntimeID}),
		"id", agentID, "serverId", serverID)
	var pending struct{ ID, Status string }
	response := testutil.Call(t, testHandler.ProbeAssignedWorkspaceMcp, start).Want(http.StatusOK)
	response.JSON(&pending)
	if pending.ID == "" || pending.Status != "pending" || strings.Contains(response.Body.String(), workspaceMcpTestSecret) {
		t.Fatal("expected a redacted pending result")
	}
	heartbeat := newDaemonTokenRequest(http.MethodPost, "/", map[string]string{"runtime_id": runtimeID}, testWorkspaceID, "probe-daemon")
	var ack protocol.DaemonHeartbeatAckPayload
	testutil.Call(t, testHandler.DaemonHeartbeat, heartbeat).Want(http.StatusOK).JSON(&ack)
	if ack.PendingMcpProbe == nil || ack.PendingMcpProbe.ID != pending.ID {
		t.Fatal("probe was not sent to the selected daemon")
	}
	daemonRequest := func(method string, body any) *http.Request {
		return withURLParams(newDaemonTokenRequest(method, "/", body, testWorkspaceID, "probe-daemon"),
			"runtimeId", runtimeID, "requestId", pending.ID)
	}
	browserJob := withURLParams(newRequest(http.MethodGet, "/", nil), "runtimeId", runtimeID, "requestId", pending.ID)
	testutil.Call(t, testHandler.GetDaemonMcpProbeJob, browserJob).Want(http.StatusNotFound)
	job := testutil.Call(t, testHandler.GetDaemonMcpProbeJob, daemonRequest(http.MethodGet, nil)).Want(http.StatusOK)
	if !strings.Contains(job.Body.String(), workspaceMcpTestSecret) {
		t.Fatal("the authenticated daemon must receive the stored credential")
	}
	report := daemonRequest(http.MethodPost, map[string]any{
		"status": "completed", "tools": []string{"search_issues", "create_issue"},
		"read_only_tools": []string{"search_issues", "not_in_list"}, "error": workspaceMcpTestSecret,
	})
	testutil.Call(t, testHandler.ReportDaemonMcpProbeResult, report).Want(http.StatusOK)

	poll := withURLParams(newRequest(http.MethodGet, "/", nil), "id", agentID, "serverId", serverID, "requestId", pending.ID)
	response = testutil.Call(t, testHandler.GetAssignedWorkspaceMcpProbe, poll).Want(http.StatusOK)
	var result struct {
		Status        string
		Tools         []string
		ReadOnlyTools []string `json:"read_only_tools"`
	}
	response.JSON(&result)
	if result.Status != "completed" || !slices.Equal(result.Tools, []string{"search_issues", "create_issue"}) || !slices.Equal(result.ReadOnlyTools, []string{"search_issues"}) ||
		strings.Contains(response.Body.String(), workspaceMcpTestSecret) {
		t.Fatal("expected a completed tool-names-only result")
	}
	dbfx.Exec(t, "UPDATE agent SET runtime_id = $1 WHERE id = $2", otherRuntimeID, agentID)
	testutil.Call(t, testHandler.GetAssignedWorkspaceMcpProbe, poll).Want(http.StatusConflict)

}
