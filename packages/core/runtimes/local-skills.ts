import { queryOptions } from "@tanstack/react-query";
import { api } from "../api";
import type {
  CreateRuntimeLocalSkillImportRequest,
  HermesSkillPreview,
  RuntimeLocalSkillImportResult,
  RuntimeLocalSkillsResult,
} from "../types";

export const runtimeLocalSkillsKeys = {
  all: () => ["runtimes", "local-skills"] as const,
  forRuntime: (runtimeId: string) =>
    [...runtimeLocalSkillsKeys.all(), runtimeId] as const,
};

const POLL_INTERVAL_MS = 500;
const POLL_TIMEOUT_MS = 30_000;
// Import timeout is longer than discovery because old daemons (pre-batch) pop
// only one import per heartbeat cycle (~15s). With 10 queued imports the 10th
// can wait up to 150s in pending before being claimed, plus up to 60s for
// the daemon to actually run the import.
//
// Timeout invariant: IMPORT_POLL_TIMEOUT_MS must exceed
// runtimeLocalSkillPendingTimeout + runtimeLocalSkillRunningTimeout
// (server/internal/handler/runtime_local_skills.go).
// See also IMPORT_CONCURRENCY in packages/views/.../runtime-local-skill-import-panel.tsx
// and maxLocalSkillImportBatch in server/internal/handler/daemon.go.
const IMPORT_POLL_TIMEOUT_MS = 4 * 60_000; // 4 minutes

async function requestRuntimeLocalSkills(
  runtimeId: string,
  agent?: { id: string; workspaceId: string; skillKey?: string },
  signal?: AbortSignal,
) {
  const initial = await api.initiateListLocalSkills(runtimeId, agent);
  const start = Date.now();
  let current = initial;

  while (current.status === "pending" || current.status === "running") {
    signal?.throwIfAborted();
    if (Date.now() - start > POLL_TIMEOUT_MS) {
      throw new Error("runtime local skill discovery timed out");
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
    signal?.throwIfAborted();
    current = await api.getListLocalSkillsResult(runtimeId, initial.id, agent?.workspaceId);
  }

  if (current.status !== "completed") {
    throw new Error(current.error || "runtime local skill discovery failed");
  }

  return current;
}

export async function resolveRuntimeLocalSkills(
  runtimeId: string,
  agent?: { id: string; workspaceId: string },
): Promise<RuntimeLocalSkillsResult> {
  const current = await requestRuntimeLocalSkills(runtimeId, agent);
  if (agent && current.agent_id !== agent.id) {
    return { skills: [], supported: false, mcpServers: [], mcpSupported: false };
  }

  return {
    skills: current.skills ?? [],
    supported: current.supported,
    mcpServers: current.mcp_servers ?? [],
    mcpSupported: current.mcp_supported === true,
  };
}

export function hermesSkillPreviewOptions(
  runtimeId: string | null,
  workspaceId: string,
  agentId: string,
  skillKey: string | null,
) {
  return queryOptions({
    queryKey: [...runtimeLocalSkillsKeys.all(), workspaceId, runtimeId, agentId, "preview", skillKey] as const,
    enabled: Boolean(runtimeId && skillKey),
    gcTime: 0,
    retry: false,
    queryFn: async ({ signal }): Promise<HermesSkillPreview> => {
      if (!runtimeId || !skillKey) throw new Error("Hermes skill selection is missing");
      const result = await requestRuntimeLocalSkills(runtimeId, { id: agentId, workspaceId, skillKey }, signal);
      if (!result.supported || result.agent_id !== agentId || result.skill_key !== skillKey || !result.preview) {
        throw new Error("Hermes skill preview is unavailable");
      }
      return result.preview;
    },
  });
}

export async function resolveRuntimeLocalSkillImport(
  runtimeId: string,
  payload: CreateRuntimeLocalSkillImportRequest,
): Promise<RuntimeLocalSkillImportResult> {
  const initial = await api.initiateImportLocalSkill(runtimeId, payload);
  const start = Date.now();
  let current = initial;

  while (current.status === "pending" || current.status === "running") {
    if (Date.now() - start > IMPORT_POLL_TIMEOUT_MS) {
      throw new Error("runtime local skill import timed out");
    }
    await new Promise((resolve) => setTimeout(resolve, POLL_INTERVAL_MS));
    current = await api.getImportLocalSkillResult(runtimeId, initial.id);
  }

  if (current.status === "conflict") {
    if (!current.conflict) {
      throw new Error("runtime local skill import conflict missing details");
    }
    return {
      status: "conflict",
      conflict: current.conflict,
    };
  }

  if (current.status === "failed" || current.status === "timeout") {
    throw new Error(current.error || "runtime local skill import failed");
  }
  if (!current.skill) {
    throw new Error("runtime local skill import did not return a skill");
  }

  return {
    status: current.action === "overwrite" ? "updated" : "created",
    skill: current.skill,
  };
}

export function runtimeLocalSkillsOptions(
  runtimeId: string | null | undefined,
  agent?: { id: string; workspaceId: string },
) {
  const queryKey = runtimeId
    ? runtimeLocalSkillsKeys.forRuntime(runtimeId)
    : runtimeLocalSkillsKeys.all();
  return queryOptions({
    queryKey: agent ? [...queryKey, agent.workspaceId, agent.id] as const : queryKey,
    queryFn: () => resolveRuntimeLocalSkills(runtimeId as string, agent),
    enabled: Boolean(runtimeId),
    staleTime: agent ? 0 : 30_000,
    retry: false,
  });
}

// The daemon returns both local skills and a redacted MCP inventory from the
// same round trip. Keep the original export for import flows and use this
// capability-oriented alias on Agent detail surfaces.
export const runtimeCapabilitiesOptions = runtimeLocalSkillsOptions;
