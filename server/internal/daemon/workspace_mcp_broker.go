package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/multica-ai/multica/server/pkg/httptransport"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"github.com/multica-ai/multica/server/pkg/remotemcp"
)

const workspaceMCPBrokerHeader = "X-Multica-Mcp-Token"

func workspaceMCPEndpoint(raw json.RawMessage) (*url.URL, http.Header, error) {
	var config struct {
		Type     string            `json:"type"`
		Command  string            `json:"command"`
		URL      string            `json:"url"`
		Headers  map[string]string `json:"headers"`
		HTTP     map[string]string `json:"http_headers"`
		Env      map[string]string `json:"env_http_headers"`
		TokenEnv string            `json:"bearer_token_env_var"`
	}
	invalid := errors.New("expected a Streamable HTTP URL and valid HTTP credentials")
	if err := json.Unmarshal(raw, &config); err != nil || config.Command != "" {
		return nil, nil, invalid
	}
	switch strings.ToLower(strings.TrimSpace(config.Type)) {
	case "", "http", "streamable-http", "remote":
	default:
		return nil, nil, invalid
	}
	endpoint, err := url.Parse(strings.TrimSpace(config.URL))
	if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.User != nil || endpoint.Fragment != "" {
		return nil, nil, invalid
	}

	headers := make(http.Header)
	if config.HTTP != nil {
		config.Headers = config.HTTP
	}
	for key, value := range config.Headers {
		headers.Set(key, value)
	}
	for key, env := range config.Env {
		value, ok := os.LookupEnv(env)
		if !ok {
			return nil, nil, errors.New("an MCP credential environment variable is missing")
		}
		headers.Set(key, value)
	}
	if config.TokenEnv != "" {
		value, ok := os.LookupEnv(config.TokenEnv)
		if !ok || value == "" {
			return nil, nil, errors.New("the MCP bearer token environment variable is missing")
		}
		headers.Set("Authorization", "Bearer "+value)
	}
	for _, key := range []string{"Accept", "Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version", "Last-Event-Id"} {
		headers.Del(key)
	}
	return endpoint, headers, nil
}

func workspaceMCPClient() *http.Client {
	return &http.Client{
		Transport: httptransport.CloneDefault(),
		Timeout:   remotemcp.CallTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func startWorkspaceMCPBrokers(ctx context.Context, assignments []protocol.RestrictedWorkspaceMCP) (json.RawMessage, *remoteMCPBrokerSet, error) {
	set := &remoteMCPBrokerSet{}
	mounts := make(map[string]map[string]any, len(assignments))
	for _, binding := range assignments {
		endpoint, headers, err := workspaceMCPEndpoint(binding.Config)
		if err != nil {
			set.Close()
			return nil, nil, fmt.Errorf("restricted MCP server %q: %w", binding.Name, err)
		}
		token, err := randomBrokerToken()
		if err != nil {
			set.Close()
			return nil, nil, err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			set.Close()
			return nil, nil, err
		}
		client := workspaceMCPClient()
		proxy := &remoteMCPProxy{
			endpoint: endpoint, client: client, path: "/mcp", credentialHeaders: headers, workspaceBrokerToken: token,
			semaphore:             make(chan struct{}, remoteMCPMaxConcurrency),
			workspaceAllowedTools: append([]string{}, binding.Tools...),
		}
		server := &http.Server{
			Handler: proxy, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
			BaseContext: func(net.Listener) context.Context { return ctx },
		}
		server.RegisterOnShutdown(client.CloseIdleConnections)
		set.servers = append(set.servers, server)
		set.listeners = append(set.listeners, listener)
		go func() { _ = server.Serve(listener) }()

		brokerURL := "http://" + listener.Addr().String() + proxy.path
		mounts[binding.Name] = map[string]any{
			"type": "http", "url": brokerURL,
			"headers": map[string]string{workspaceMCPBrokerHeader: token},
		}
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": mounts})
	if err != nil {
		set.Close()
		return nil, nil, err
	}
	return raw, set, nil
}

func canonicalWorkspaceMCPRequest(raw []byte, request *remoteMCPRequest) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	// Read exact JSON-RPC field names and emit each once. The same forwarded
	// name is used by the permission check.
	raw, err := json.Marshal(map[string]json.RawMessage{
		"jsonrpc": fields["jsonrpc"], "id": fields["id"], "method": fields["method"], "params": fields["params"],
	})
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, request); err != nil {
		return nil, err
	}
	if bytes.Equal(request.ID, []byte("null")) {
		request.ID = nil
	}
	if request.Method == "tools/call" {
		var params map[string]json.RawMessage
		var name string
		if json.Unmarshal(request.Params, &params) != nil || json.Unmarshal(params["name"], &name) != nil || name == "" {
			return nil, errors.New("invalid tool name")
		}
		canonical := map[string]json.RawMessage{"name": params["name"]}
		for _, key := range []string{"arguments", "_meta"} {
			if value, ok := params[key]; ok {
				canonical[key] = value
			}
		}
		request.Params, err = json.Marshal(canonical)
		if err != nil {
			return nil, err
		}
	} else if bytes.Equal(request.Params, []byte("null")) {
		request.Params = nil
	}
	return json.Marshal(request)
}

func (proxy *remoteMCPProxy) endWorkspaceSession(ctx context.Context, session, version string) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, proxy.endpoint.String(), nil)
	if err != nil {
		return
	}
	req.Header = proxy.credentialHeaders.Clone()
	req.Header.Set("Mcp-Session-Id", session)
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	if response, err := proxy.client.Do(req); err == nil {
		_ = response.Body.Close()
	}
}

func (proxy *remoteMCPProxy) writeWorkspaceResponse(w http.ResponseWriter, req remoteMCPRequest, response *http.Response) {
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		writeRemoteMCPError(w, req.ID, -32000, "Assigned MCP server returned an error")
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	body, err := workspaceMCPResponse(response, req.ID)
	if err == nil && req.Method == "tools/list" {
		body, err = proxy.filterWorkspaceTools(body)
	}
	if err != nil {
		writeRemoteMCPError(w, req.ID, -32000, "Assigned MCP server returned an invalid response")
		return
	}
	for _, key := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if value := response.Header.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (proxy *remoteMCPProxy) filterWorkspaceTools(body []byte) ([]byte, error) {
	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, err
	}
	if len(response["error"]) > 0 && !bytes.Equal(response["error"], []byte("null")) {
		delete(response, "result")
		return json.Marshal(response)
	}
	delete(response, "error")
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response["result"], &result); err != nil {
		return nil, err
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(result["tools"], &tools); err != nil || tools == nil {
		return nil, errors.New("invalid tools list")
	}
	filtered := make([]json.RawMessage, 0, len(tools))
	for _, tool := range tools {
		var fields map[string]json.RawMessage
		var name string
		if json.Unmarshal(tool, &fields) != nil || json.Unmarshal(fields["name"], &name) != nil || !proxy.toolApproved(name) {
			continue
		}
		tool, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		filtered = append(filtered, tool)
	}
	result["tools"], _ = json.Marshal(filtered)
	response["result"], _ = json.Marshal(result)
	return json.Marshal(response)
}

// Adapted from upstream #9290. Read only the response for this RPC, since an
// SSE connection may send notifications before it or remain open afterward.
func workspaceMCPResponse(response *http.Response, id json.RawMessage) ([]byte, error) {
	body := io.LimitReader(response.Body, remotemcp.MaxResponseBytes+1)
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		raw, err := io.ReadAll(body)
		if len(raw) > remotemcp.MaxResponseBytes {
			return nil, errors.New("MCP response exceeds size limit")
		}
		return raw, err
	}

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 4096), remotemcp.MaxResponseBytes)
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(line[5:], " "))
		} else if line == "" && len(data) > 0 {
			raw := []byte(strings.Join(data, "\n"))
			data = nil
			var message struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			if json.Unmarshal(raw, &message) == nil && message.Method == "" && bytes.Equal(message.ID, id) {
				return raw, nil
			}
		}
	}
	return nil, errors.New("MCP stream ended before the response")
}

func (proxy *remoteMCPProxy) sendWorkspaceRequest(ctx context.Context, rpc remoteMCPRequest, session, version string) (*http.Response, error) {
	body, err := json.Marshal(rpc)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = proxy.credentialHeaders.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("Mcp-Protocol-Version", version)
	}
	return proxy.client.Do(req)
}
