package handler

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	obsmetrics "github.com/multica-ai/multica/server/internal/metrics"
	"github.com/multica-ai/multica/server/internal/middleware"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// McpProbeStatus is the lifecycle of one on-demand probe request.
// The result lives only as long as this request. It is not written back
// onto the library row (GH #7166: persisted last_probe is a later slice).
type McpProbeStatus string

const (
	McpProbePending   McpProbeStatus = "pending"
	McpProbeRunning   McpProbeStatus = "running"
	McpProbeCompleted McpProbeStatus = "completed"
	McpProbeFailed    McpProbeStatus = "failed"
	McpProbeTimeout   McpProbeStatus = "timeout"
)

const (
	mcpProbePendingTimeout = 30 * time.Second
	mcpProbeRunningTimeout = 60 * time.Second
	mcpProbeStoreRetention = 2 * time.Minute
)

// McpProbeRequest is the pollable in-flight (or just-finished) probe.
type McpProbeRequest struct {
	ID            string         `json:"id"`
	AgentID       string         `json:"agent_id"`
	RequesterID   string         `json:"requester_id"`
	ConfigVersion time.Time      `json:"config_version"`
	WorkspaceID   string         `json:"workspace_id"`
	ServerID      string         `json:"server_id"`
	RuntimeID     string         `json:"runtime_id"`
	Status        McpProbeStatus `json:"status"`
	ErrorCode     string         `json:"error_code,omitempty"`
	Tools         []string       `json:"tools,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	RunStartedAt  *time.Time     `json:"-"`
}

// McpProbeCreate is the store input for a newly queued probe.
type McpProbeCreate struct {
	AgentID       string
	RequesterID   string
	ConfigVersion time.Time
	WorkspaceID   string
	ServerID      string
	RuntimeID     string
}

// McpProbeReport is what the daemon posts back.
type McpProbeReport struct {
	Status    string   `json:"status"` // "completed" or "failed"
	ErrorCode string   `json:"error_code,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// McpProbeStore is the multi-replica contract for in-flight probes.
type McpProbeStore interface {
	Create(ctx context.Context, in McpProbeCreate) (*McpProbeRequest, error)
	Get(ctx context.Context, id string) (*McpProbeRequest, error)
	HasPending(ctx context.Context, runtimeID string) (bool, error)
	PopPending(ctx context.Context, runtimeID string) (*McpProbeRequest, error)
	Complete(ctx context.Context, id string, tools []string) error
	Fail(ctx context.Context, id string, code string) error
}

func applyMcpProbeTimeout(req *McpProbeRequest, now time.Time) bool {
	switch req.Status {
	case McpProbePending:
		if now.Sub(req.CreatedAt) > mcpProbePendingTimeout {
			req.Status = McpProbeTimeout
			req.ErrorCode = protocol.McpProbeCodeTimeout
			req.UpdatedAt = now
			return true
		}
	case McpProbeRunning:
		if req.RunStartedAt != nil && now.Sub(*req.RunStartedAt) > mcpProbeRunningTimeout {
			req.Status = McpProbeTimeout
			req.ErrorCode = protocol.McpProbeCodeTimeout
			req.UpdatedAt = now
			return true
		}
	}
	return false
}

func mcpProbeRequestTerminal(status McpProbeStatus) bool {
	return status == McpProbeCompleted || status == McpProbeFailed || status == McpProbeTimeout
}

// InMemoryMcpProbeStore is the single-node implementation.
type InMemoryMcpProbeStore struct {
	mu       sync.Mutex
	requests map[string]*McpProbeRequest
}

func NewInMemoryMcpProbeStore() *InMemoryMcpProbeStore {
	return &InMemoryMcpProbeStore{requests: make(map[string]*McpProbeRequest)}
}

func (s *InMemoryMcpProbeStore) Create(_ context.Context, in McpProbeCreate) (*McpProbeRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, req := range s.requests {
		if time.Since(req.CreatedAt) > mcpProbeStoreRetention {
			delete(s.requests, id)
		}
	}
	now := time.Now()
	req := &McpProbeRequest{
		ID:            randomID(),
		WorkspaceID:   in.WorkspaceID,
		AgentID:       in.AgentID,
		RequesterID:   in.RequesterID,
		ConfigVersion: in.ConfigVersion,
		ServerID:      in.ServerID,
		RuntimeID:     in.RuntimeID,
		Status:        McpProbePending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	s.requests[req.ID] = req
	return cloneMcpProbeRequest(req), nil
}

func (s *InMemoryMcpProbeStore) Get(_ context.Context, id string) (*McpProbeRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	req, ok := s.requests[id]
	if !ok {
		return nil, nil
	}
	if time.Since(req.CreatedAt) > mcpProbeStoreRetention {
		delete(s.requests, id)
		return nil, nil
	}
	applyMcpProbeTimeout(req, time.Now())
	return cloneMcpProbeRequest(req), nil
}

func (s *InMemoryMcpProbeStore) HasPending(_ context.Context, runtimeID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for _, req := range s.requests {
		applyMcpProbeTimeout(req, now)
		if req.RuntimeID == runtimeID && req.Status == McpProbePending {
			return true, nil
		}
	}
	return false, nil
}

func (s *InMemoryMcpProbeStore) PopPending(_ context.Context, runtimeID string) (*McpProbeRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var oldest *McpProbeRequest
	now := time.Now()
	for _, req := range s.requests {
		applyMcpProbeTimeout(req, now)
		if req.RuntimeID == runtimeID && req.Status == McpProbePending {
			if oldest == nil || req.CreatedAt.Before(oldest.CreatedAt) {
				oldest = req
			}
		}
	}
	if oldest != nil {
		oldest.Status = McpProbeRunning
		started := now
		oldest.RunStartedAt = &started
		oldest.UpdatedAt = now
	}
	return cloneMcpProbeRequest(oldest), nil
}

func cloneMcpProbeRequest(req *McpProbeRequest) *McpProbeRequest {
	if req == nil {
		return nil
	}
	copy := *req
	copy.Tools = append([]string(nil), req.Tools...)
	if req.RunStartedAt != nil {
		started := *req.RunStartedAt
		copy.RunStartedAt = &started
	}
	return &copy
}

func (s *InMemoryMcpProbeStore) Complete(_ context.Context, id string, tools []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req, ok := s.requests[id]; ok {
		applyMcpProbeTimeout(req, time.Now())
		if mcpProbeRequestTerminal(req.Status) {
			return nil
		}
		req.Status = McpProbeCompleted
		req.Tools = append([]string(nil), tools...)
		req.ErrorCode = ""
		req.UpdatedAt = time.Now()
	}
	return nil
}

func (s *InMemoryMcpProbeStore) Fail(_ context.Context, id string, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req, ok := s.requests[id]; ok {
		applyMcpProbeTimeout(req, time.Now())
		if mcpProbeRequestTerminal(req.Status) {
			return nil
		}
		req.Status = McpProbeFailed
		req.ErrorCode = sanitizeMcpProbeErrorCode(code)
		req.UpdatedAt = time.Now()
	}
	return nil
}

func sanitizeMcpProbeErrorCode(code string) string {
	code = strings.TrimSpace(code)
	switch code {
	case protocol.McpProbeCodeTimeout, protocol.McpProbeCodeUnauthorized,
		protocol.McpProbeCodeUnsupportedTransport, protocol.McpProbeCodeHandshake, protocol.McpProbeCodeInternal:
		return code
	default:
		return protocol.McpProbeCodeInternal
	}
}

func (h *Handler) ProbeAssignedWorkspaceMcp(w http.ResponseWriter, r *http.Request) {
	agent, server, ok := h.loadAssignedMcpForEdit(w, r)
	if !ok {
		return
	}
	if mcpTransportOf(server.Config) != "http" {
		writeError(w, http.StatusBadRequest, "tool discovery requires a Streamable HTTP server")
		return
	}
	var body struct {
		RuntimeID string `json:"runtime_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	runtimeID, ok := parseUUIDOrBadRequest(w, body.RuntimeID, "runtime_id")
	if !ok {
		return
	}
	callerID := requestUserID(r)
	rt, err := h.getAgentRuntime(r.Context(), obsmetrics.RuntimeLookupSourceRuntimeAPI, runtimeID)
	if err != nil || rt.WorkspaceID != agent.WorkspaceID || !runtimeOwnedBy(rt, callerID) {
		writeError(w, http.StatusNotFound, "runtime not found")
		return
	}
	if rt.Status != "online" || !workspaceMCPRuntimeSupported(rt) ||
		!runtimeHasCapability(rt.Metadata, protocol.WorkspaceMCPToolsCapability(rt.Provider)) {
		writeError(w, http.StatusConflict, "select an online local Codex or Hermes runtime with an updated Multica daemon")
		return
	}
	req, err := h.McpProbeStore.Create(r.Context(), McpProbeCreate{
		WorkspaceID:   uuidToString(agent.WorkspaceID),
		AgentID:       uuidToString(agent.ID),
		RequesterID:   callerID,
		ServerID:      uuidToString(server.ID),
		ConfigVersion: server.UpdatedAt.Time,
		RuntimeID:     uuidToString(rt.ID),
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to schedule MCP tool discovery")
		return
	}
	h.requestDaemonPendingWork(req.RuntimeID, protocol.PendingWorkKindMcpProbe)
	writeMcpProbeResult(w, req)
}

func (h *Handler) GetAssignedWorkspaceMcpProbe(w http.ResponseWriter, r *http.Request) {
	agent, server, ok := h.loadAssignedMcpForEdit(w, r)
	if !ok {
		return
	}
	req, err := h.McpProbeStore.Get(r.Context(), chi.URLParam(r, "requestId"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load MCP probe")
		return
	}
	if req == nil || req.AgentID != uuidToString(agent.ID) || req.ServerID != uuidToString(server.ID) ||
		req.RequesterID != requestUserID(r) {
		writeError(w, http.StatusNotFound, "probe not found")
		return
	}
	if !server.UpdatedAt.Time.Equal(req.ConfigVersion) {
		writeError(w, http.StatusConflict, "MCP configuration changed; reload the tool list")
		return
	}
	writeMcpProbeResult(w, req)
}

func writeMcpProbeResult(w http.ResponseWriter, req *McpProbeRequest) {
	writeJSON(w, http.StatusOK, struct {
		ID        string         `json:"id"`
		Status    McpProbeStatus `json:"status"`
		ErrorCode string         `json:"error_code,omitempty"`
		Tools     []string       `json:"tools"`
	}{req.ID, req.Status, req.ErrorCode, append([]string{}, req.Tools...)})
}

// requireDaemonMcpProbeCaller allows mdt_ and PAT (how daemons auth today)
// and 404s browser JWT / direct handler calls so write-only config never
// leaves on a user session.
func (h *Handler) requireDaemonMcpProbeCaller(w http.ResponseWriter, r *http.Request) bool {
	if middleware.DaemonIDFromContext(r.Context()) != "" {
		return true
	}
	switch middleware.DaemonAuthPathFromContext(r.Context()) {
	case middleware.DaemonAuthPathPAT, middleware.DaemonAuthPathCloudPAT, middleware.DaemonAuthPathDaemonToken:
		return true
	default:
		writeError(w, http.StatusNotFound, "not found")
		return false
	}
}

// GetDaemonMcpProbeJob returns the write-only config to the daemon that will
// run the handshake. Browser JWT is rejected here; live daemons still
// authenticate with PAT or mdt_ on /api/daemon.
func (h *Handler) GetDaemonMcpProbeJob(w http.ResponseWriter, r *http.Request) {
	if !h.requireDaemonMcpProbeCaller(w, r) {
		return
	}
	runtimeID := chi.URLParam(r, "runtimeId")
	rt, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
	if !ok || !mcpProbeDaemonOwnsRuntime(r, rt) {
		if ok {
			writeError(w, http.StatusNotFound, "runtime not found")
		}
		return
	}
	requestID := chi.URLParam(r, "requestId")
	if h.McpProbeStore == nil {
		writeError(w, http.StatusInternalServerError, "probe store is not configured")
		return
	}
	req, err := h.McpProbeStore.Get(r.Context(), requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load request")
		return
	}
	if req == nil || req.RuntimeID != runtimeID {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if req.Status != McpProbeRunning {
		writeError(w, http.StatusConflict, "probe is not running")
		return
	}
	if req.WorkspaceID != uuidToString(rt.WorkspaceID) || !runtimeOwnedBy(rt, req.RequesterID) || !workspaceMCPRuntimeSupported(rt) {
		writeError(w, http.StatusNotFound, "probe not found")
		return
	}
	server, err := h.Queries.GetAssignedWorkspaceMcpServer(r.Context(), db.GetAssignedWorkspaceMcpServerParams{
		ID: parseUUID(req.ServerID), WorkspaceID: rt.WorkspaceID, AgentID: parseUUID(req.AgentID),
	})
	if err != nil || !server.UpdatedAt.Time.Equal(req.ConfigVersion) || mcpTransportOf(server.Config) != "http" {
		writeError(w, http.StatusConflict, "MCP assignment or configuration changed; reload the tool list")
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	writeJSON(w, http.StatusOK, map[string]any{
		"request_id":  req.ID,
		"server_name": server.Name,
		"config":      json.RawMessage(server.Config),
	})
}

// ReportDaemonMcpProbeResult receives the handshake outcome from the daemon.
func (h *Handler) ReportDaemonMcpProbeResult(w http.ResponseWriter, r *http.Request) {
	if !h.requireDaemonMcpProbeCaller(w, r) {
		return
	}
	runtimeID := chi.URLParam(r, "runtimeId")
	rt, ok := h.requireDaemonRuntimeAccess(w, r, runtimeID)
	if !ok || !mcpProbeDaemonOwnsRuntime(r, rt) {
		if ok {
			writeError(w, http.StatusNotFound, "runtime not found")
		}
		return
	}
	requestID := chi.URLParam(r, "requestId")
	if h.McpProbeStore == nil {
		writeError(w, http.StatusInternalServerError, "probe store is not configured")
		return
	}
	existing, err := h.McpProbeStore.Get(r.Context(), requestID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load request")
		return
	}
	if existing == nil || existing.RuntimeID != runtimeID || !runtimeOwnedBy(rt, existing.RequesterID) || existing.WorkspaceID != uuidToString(rt.WorkspaceID) {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}
	if mcpProbeRequestTerminal(existing.Status) {
		slog.Debug("ignoring stale mcp probe report", "runtime_id", runtimeID, "request_id", requestID, "status", existing.Status)
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	var body McpProbeReport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Status != "completed" && body.Status != "failed" {
		writeError(w, http.StatusBadRequest, "invalid probe status")
		return
	}
	if body.Status == "completed" {
		if err := h.McpProbeStore.Complete(r.Context(), requestID, sanitizeMcpProbeTools(body.Tools)); err != nil {
			slog.Error("McpProbeStore Complete failed", "error", err, "request_id", requestID)
			writeError(w, http.StatusInternalServerError, "failed to persist completion")
			return
		}
	} else {
		if err := h.McpProbeStore.Fail(r.Context(), requestID, body.ErrorCode); err != nil {
			slog.Error("McpProbeStore Fail failed", "error", err, "request_id", requestID)
			writeError(w, http.StatusInternalServerError, "failed to persist failure")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func mcpProbeDaemonOwnsRuntime(r *http.Request, rt db.AgentRuntime) bool {
	if daemonID := middleware.DaemonIDFromContext(r.Context()); daemonID != "" {
		return rt.DaemonID.Valid && rt.DaemonID.String == daemonID
	}
	return runtimeOwnedBy(rt, requestUserID(r))
}

func sanitizeMcpProbeTools(tools []string) []string {
	out := make([]string, 0, len(tools))
	seen := make(map[string]struct{}, len(tools))
	for _, name := range tools {
		if !protocol.WorkspaceMCPToolName.MatchString(name) {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
		if len(out) >= protocol.MaxWorkspaceMCPTools {
			break
		}
	}
	return out
}

func runtimeOwnedBy(rt db.AgentRuntime, userID string) bool {
	return userID != "" && rt.OwnerID.Valid && uuidToString(rt.OwnerID) == userID
}
