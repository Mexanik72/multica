package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/multica-ai/multica/server/pkg/protocol"
)

func (d *Daemon) handleWorkspaceMCPProbe(ctx context.Context, runtimeID, requestID string) {
	jobPath := fmt.Sprintf("/api/daemon/runtimes/%s/mcp-probes/%s", runtimeID, requestID)
	outcome := struct {
		Status    string   `json:"status"`
		ErrorCode string   `json:"error_code,omitempty"`
		Tools     []string `json:"tools"`
	}{Status: "failed", ErrorCode: protocol.McpProbeCodeInternal}

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var job struct {
		Config json.RawMessage `json:"config"`
	}
	if err := d.client.getJSON(probeCtx, jobPath, &job); err == nil {
		rt := d.findRuntime(runtimeID)
		if rt != nil && rt.Provider == "codex" && rt.ProfileID == "" {
			outcome.Tools, outcome.ErrorCode = probeWorkspaceMCP(probeCtx, job.Config)
			if outcome.ErrorCode == "" {
				outcome.Status = "completed"
			}
		}
	}
	reportCtx, cancelReport := context.WithTimeout(ctx, 5*time.Second)
	defer cancelReport()
	if err := d.client.postJSON(reportCtx, jobPath+"/result", outcome, nil); err != nil {
		d.logger.Warn("could not report MCP probe result", "request_id", requestID)
	}
}

// Uses the on-demand initialize/list sequence from upstream #9290, with
// the same endpoint parsing and HTTP transport as the task broker.
func probeWorkspaceMCP(ctx context.Context, raw json.RawMessage) ([]string, string) {
	endpoint, headers, err := workspaceMCPEndpoint(raw)
	if err != nil {
		return nil, protocol.McpProbeCodeUnsupportedTransport
	}
	client := workspaceMCPClient()
	defer client.CloseIdleConnections()

	proxy := &remoteMCPProxy{endpoint: endpoint, client: client, credentialHeaders: headers}
	var session, version string
	defer func() {
		if session != "" {
			proxy.endWorkspaceSession(context.WithoutCancel(ctx), session, version)
		}
	}()

	seq := 0
	call := func(method string, params any, notification bool) (json.RawMessage, string) {
		req := remoteMCPRequest{JSONRPC: "2.0", Method: method}
		req.Params, _ = json.Marshal(params)
		if !notification {
			seq++
			req.ID, _ = json.Marshal(seq)
		}
		response, err := proxy.sendWorkspaceRequest(ctx, req, session, version)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				return nil, protocol.McpProbeCodeTimeout
			}
			return nil, protocol.McpProbeCodeHandshake
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, protocol.McpProbeCodeUnauthorized
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return nil, protocol.McpProbeCodeHandshake
		}
		if header := response.Header.Get("Mcp-Session-Id"); header != "" {
			session = header
		}
		if notification {
			return nil, ""
		}

		raw, err := workspaceMCPResponse(response, req.ID)
		var parsed struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Result  json.RawMessage `json:"result"`
		}
		if err != nil || json.Unmarshal(raw, &parsed) != nil ||
			parsed.JSONRPC != "2.0" || parsed.ID != seq || len(parsed.Result) == 0 {
			return nil, protocol.McpProbeCodeHandshake
		}
		return parsed.Result, ""
	}

	initialized, code := call("initialize", map[string]any{
		"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "multica-probe", "version": "1"},
	}, false)
	if code != "" {
		return nil, code
	}
	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(initialized, &initResult) != nil {
		return nil, protocol.McpProbeCodeHandshake
	}
	switch initResult.ProtocolVersion {
	case "2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25":
		version = initResult.ProtocolVersion
	default:
		return nil, protocol.McpProbeCodeHandshake
	}
	if _, code := call("notifications/initialized", map[string]any{}, true); code != "" {
		return nil, code
	}
	names := make([]string, 0)
	params := map[string]string{}
	seenCursors := map[string]bool{}
	for {
		raw, code := call("tools/list", params, false)
		if code != "" {
			return nil, code
		}
		var page struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if json.Unmarshal(raw, &page) != nil || page.Tools == nil {
			return nil, protocol.McpProbeCodeHandshake
		}
		for _, tool := range page.Tools {
			if !protocol.WorkspaceMCPToolName.MatchString(tool.Name) {
				return nil, protocol.McpProbeCodeHandshake
			}
			names = append(names, tool.Name)
		}
		if len(names) > protocol.MaxWorkspaceMCPTools || len(seenCursors) >= 100 || seenCursors[page.NextCursor] {
			return nil, protocol.McpProbeCodeHandshake
		}
		if page.NextCursor == "" {
			return names, ""
		}
		seenCursors[page.NextCursor] = true
		params["cursor"] = page.NextCursor
	}
}
