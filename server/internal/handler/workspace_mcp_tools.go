package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (h *Handler) loadAssignedMcpForEdit(w http.ResponseWriter, r *http.Request) (db.Agent, db.WorkspaceMcpServer, bool) {
	agent, ok := h.requireAgentMcpWriter(w, r)
	if !ok {
		return db.Agent{}, db.WorkspaceMcpServer{}, false
	}
	serverID, ok := parseUUIDOrBadRequest(w, chi.URLParam(r, "serverId"), "server id")
	if !ok {
		return db.Agent{}, db.WorkspaceMcpServer{}, false
	}
	server, err := h.Queries.GetAssignedWorkspaceMcpServer(r.Context(), db.GetAssignedWorkspaceMcpServerParams{
		ID: serverID, WorkspaceID: agent.WorkspaceID, AgentID: agent.ID,
	})
	if err != nil {
		if isNotFound(err) {
			writeError(w, http.StatusNotFound, "this MCP server is not assigned to the agent")
		} else {
			writeError(w, http.StatusInternalServerError, "failed to load the MCP server assignment")
		}
		return db.Agent{}, db.WorkspaceMcpServer{}, false
	}
	return agent, server, true
}

func (h *Handler) SetAgentMcpToolAccess(w http.ResponseWriter, r *http.Request) {
	agent, server, ok := h.loadAssignedMcpForEdit(w, r)
	if !ok {
		return
	}
	var body struct {
		Mode  string   `json:"mode"`
		Tools []string `json:"tools"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch body.Mode {
	case "all":
		if len(body.Tools) != 0 {
			writeError(w, http.StatusBadRequest, "all mode takes no tool names")
			return
		}
		body.Tools = nil
	case "selected":
		if mcpTransportOf(server.Config) != "http" {
			writeError(w, http.StatusBadRequest, "tool restrictions require a Streamable HTTP server")
			return
		}
		if body.Tools == nil || len(body.Tools) > protocol.MaxWorkspaceMCPTools {
			writeError(w, http.StatusBadRequest, "selected mode requires a tools array of at most 1000 names")
			return
		}
		for _, tool := range body.Tools {
			if !protocol.WorkspaceMCPToolName.MatchString(tool) {
				writeError(w, http.StatusBadRequest, "invalid MCP tool name")
				return
			}
		}
		body.Tools = sanitizeMcpProbeTools(body.Tools)
	default:
		writeError(w, http.StatusBadRequest, "mode must be all or selected")
		return
	}
	rows, err := h.Queries.SetAgentMcpAllowedTools(r.Context(), db.SetAgentMcpAllowedToolsParams{
		AgentID: agent.ID, ServerID: server.ID, AllowedTools: body.Tools,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to update MCP tool access")
		return
	}
	if rows == 0 {
		writeError(w, http.StatusNotFound, "this MCP server is not assigned to the agent")
		return
	}
	h.writeAgentMcpServers(w, r, agent.ID)
}

func workspaceMCPRuntimeSupported(runtime db.AgentRuntime) bool {
	return runtime.Provider == "hermes" && runtime.RuntimeMode == "local" && !runtime.ProfileID.Valid
}

func splitWorkspaceMCPBindings(bound []db.ListEnabledAgentMcpServersRow, runtime db.AgentRuntime, capable bool, configs ...json.RawMessage) ([]WorkspaceMcpBinding, []protocol.RestrictedWorkspaceMCP, error) {
	bindings := make([]WorkspaceMcpBinding, 0, len(bound))
	var restricted []protocol.RestrictedWorkspaceMCP
	for _, server := range bound {
		if server.AllowedTools == nil {
			bindings = append(bindings, WorkspaceMcpBinding{Name: server.Name, Config: server.Config})
			continue
		}
		if !workspaceMCPRuntimeSupported(runtime) || mcpTransportOf(server.Config) != "http" {
			return nil, nil, fmt.Errorf("restricted MCP server %q requires a local built-in Hermes runtime and Streamable HTTP", server.Name)
		}
		if !capable {
			return nil, nil, fmt.Errorf("update the Multica daemon to run with restricted MCP server %q", server.Name)
		}
		restricted = append(restricted, protocol.RestrictedWorkspaceMCP{Name: server.Name, Config: server.Config, Tools: server.AllowedTools})
	}
	if len(restricted) == 0 {
		return bindings, nil, nil
	}
	for _, raw := range configs {
		if !hasManagedJSON(raw) {
			continue
		}
		_, names, err := parseMcpDocument(raw)
		if err != nil {
			return nil, nil, errors.New("cannot verify names for restricted MCP servers because the MCP configuration is invalid")
		}
		for _, server := range restricted {
			if _, collision := names[server.Name]; collision {
				return nil, nil, fmt.Errorf("restricted MCP server %q conflicts with another server in the agent or task configuration; rename one of the servers", server.Name)
			}
		}
	}
	return bindings, restricted, nil
}
