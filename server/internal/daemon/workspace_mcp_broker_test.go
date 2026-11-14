package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/multica-ai/multica/server/internal/daemon/execenv"
	"github.com/multica-ai/multica/server/pkg/protocol"
	"gopkg.in/yaml.v3"
)

func TestWorkspaceMCPProxyEnforcesAssignment(t *testing.T) {
	upstreamCalls := make(chan string, 5)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var call remoteMCPRequest
		if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		if call.Method == "tools/call" {
			upstreamCalls <- string(call.Params)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"content":[]}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{\"nextCursor\":\"next\",\"tools\":[{\"name\":\"search_issues\"},{\"name\":\"create_issue\"},{\"name\":\"new_tool\"}]}}\n\n"))
	}))
	defer upstream.Close()
	endpoint, _ := url.Parse(upstream.URL)
	for _, allowed := range [][]string{{"search_issues"}, {"search_issues", "create_issue"}, {}} {
		proxy := &remoteMCPProxy{
			endpoint: endpoint, client: upstream.Client(), path: "/assignment",
			semaphore: make(chan struct{}, 1), workspaceAllowedTools: allowed,
		}
		post := func(body string) string {
			w := httptest.NewRecorder()
			proxy.ServeHTTP(w, httptest.NewRequest(http.MethodPost, proxy.path, strings.NewReader(body)))
			return w.Body.String()
		}
		var listed struct {
			Result struct {
				Tools      []struct{ Name string }
				NextCursor string
			}
		}
		raw := post(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if err := json.Unmarshal([]byte(raw), &listed); err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(listed.Result.Tools))
		for _, tool := range listed.Result.Tools {
			names = append(names, tool.Name)
		}
		if !reflect.DeepEqual(names, allowed) || listed.Result.NextCursor != "next" {
			t.Fatalf("policy %v returned %s", allowed, raw)
		}
		for _, name := range []string{"search_issues", "create_issue", "new_tool"} {
			raw := post(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"` + name + `"}}`)
			permitted := len(allowed) > 0 && (name == "search_issues" || len(allowed) == 2 && name == "create_issue")
			select {
			case <-upstreamCalls:
				if !permitted || !strings.Contains(raw, "result") {
					t.Fatalf("unexpected upstream call for %s with policy %v: %s", name, allowed, raw)
				}
			default:
				if permitted || !strings.Contains(raw, "not approved") {
					t.Fatalf("unexpected rejection for %s with policy %v: %s", name, allowed, raw)
				}
			}
		}
	}
}

func TestWorkspaceMCPListErrorCannotExposeUnfilteredTools(t *testing.T) {
	proxy := &remoteMCPProxy{workspaceAllowedTools: []string{"search_issues"}}
	body, err := proxy.filterWorkspaceTools([]byte(`{
		"jsonrpc":"2.0","id":1,"error":null,"result":{"tools":[{"name":"create_issue"}]}
	}`))
	if err != nil || strings.Contains(string(body), "create_issue") {
		t.Fatalf("list exposed a restricted tool: %s, %v", body, err)
	}
}

func TestWorkspaceMCPProbeListsPagesAndClosesSession(t *testing.T) {
	var closed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-credential" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			closed.Store(r.Header.Get("Mcp-Session-Id") == "session-1")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req remoteMCPRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		var result string
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "session-1")
			result = `{"protocolVersion":"2025-03-26"}`
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			if r.Header.Get("Mcp-Session-Id") != "session-1" || r.Header.Get("Mcp-Protocol-Version") != "2025-03-26" {
				t.Error("missing negotiated session or protocol version")
			}
			if strings.Contains(string(req.Params), "next-page") {
				result = `{"tools":[{"name":"create_issue"}]}`
			} else {
				result = `{"tools":[{"name":"search_issues","annotations":{"readOnlyHint":true},"description":"description stays upstream"}],"nextCursor":"next-page"}`
			}
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n\n", req.ID, result)
	}))
	defer server.Close()

	raw, _ := json.Marshal(map[string]any{
		"url": server.URL, "headers": map[string]string{"Authorization": "Bearer test-credential"},
	})
	tools, code := probeWorkspaceMCP(t.Context(), raw)
	if code != "" || !reflect.DeepEqual(tools.Tools, []string{"search_issues", "create_issue"}) || !reflect.DeepEqual(tools.ReadOnlyTools, []string{"search_issues"}) || !closed.Load() {
		t.Fatalf("tools=%v error=%s session closed=%v", tools, code, closed.Load())
	}
}

func TestWorkspaceMCPHermesConfigUsesBrokerInsteadOfMachineServer(t *testing.T) {
	var toolCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var call remoteMCPRequest
		if err := json.NewDecoder(req.Body).Decode(&call); err != nil {
			t.Error(err)
			return
		}
		if call.Method == "tools/call" {
			toolCalls.Add(1)
			if !strings.Contains(string(call.Params), "search_issues") {
				t.Error("forbidden tool reached the upstream")
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"found issues"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search_issues"},{"name":"create_issue"}]}}`))
	}))
	defer upstream.Close()

	raw, _ := json.Marshal(map[string]string{"url": upstream.URL})
	mounts, brokers, err := startWorkspaceMCPBrokers(t.Context(), []protocol.RestrictedWorkspaceMCP{
		{Name: "issues", Config: raw, Tools: []string{"search_issues"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer brokers.Close()
	home := t.TempDir()
	path := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(path, []byte("mcp_servers:\n  issues: {command: machine-command}\n  other: {command: keep}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := execenv.ConfigureHermesWorkspaceMCP(home, mounts); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "machine-command") || strings.Contains(string(data), upstream.URL) ||
		!strings.Contains(string(data), "command: keep") {
		t.Fatal("task config must replace the collision and preserve unrelated servers")
	}
	var config struct {
		Servers map[string]struct {
			URL     string
			Headers map[string]string
		} `yaml:"mcp_servers"`
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	cfg := config.Servers["issues"]
	for _, test := range []struct{ request, want, absent string }{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, "search_issues", "create_issue"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search_issues"}}`, "found issues", "error"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"create_issue"}}`, "not approved", "result"},
	} {
		req, err := http.NewRequest(http.MethodPost, cfg.URL, strings.NewReader(test.request))
		if err != nil {
			t.Fatal(err)
		}
		for key, value := range cfg.Headers {
			req.Header.Set(key, value)
		}
		resp, err := upstream.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !strings.Contains(string(body), test.want) || strings.Contains(string(body), test.absent) {
			t.Fatalf("unexpected broker response %s, %v", body, err)
		}
	}
	if got := toolCalls.Load(); got != 1 {
		t.Fatalf("expected one upstream tool call, got %d", got)
	}
}
