"use client";

import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import { runtimeDisplayLabel, runtimeListOptions } from "@multica/core/runtimes";
import type { AgentRuntime, WorkspaceMcpServer } from "@multica/core/types";
import { useSetAgentMcpToolAccess, useStartAgentMcpProbe } from "@multica/core/workspace/mutations";
import { Button } from "@multica/ui/components/ui/button";
import { Checkbox } from "@multica/ui/components/ui/checkbox";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@multica/ui/components/ui/dialog";
import { Loader2 } from "@multica/ui/icons";
import { useT } from "../../../i18n";

function supportsMcpToolAccess(runtime: AgentRuntime | null) {
  return runtime?.runtime_mode === "local" && runtime.provider === "codex" &&
    !runtime.profile_id && Array.isArray(runtime.metadata?.capabilities) &&
    runtime.metadata.capabilities.includes("workspace-mcp-tools-v1");
}

export function McpToolAccessDialog(props: {
  agentId: string;
  workspaceId: string;
  server: WorkspaceMcpServer;
  runtime: AgentRuntime | null;
  disabled: boolean;
}) {
  const { t } = useT("agents");
  const [open, setOpen] = useState(false);
  const save = useSetAgentMcpToolAccess(props.agentId);
  return (
    <Dialog open={open} onOpenChange={(next) => { if (!save.isPending) setOpen(next); }}>
      <DialogTrigger render={<Button size="xs" variant="ghost" disabled={props.disabled} />}>
        {t(($) => $.tab_body.mcp_config.tool_access.action)}
      </DialogTrigger>
      <DialogContent>
        {open && <McpToolAccessForm {...props} save={save} onSaved={() => setOpen(false)} />}
      </DialogContent>
    </Dialog>
  );
}

function McpToolAccessForm({
  agentId, workspaceId, server, runtime, save, onSaved,
}: {
  agentId: string;
  workspaceId: string;
  server: WorkspaceMcpServer;
  runtime: AgentRuntime | null;
  save: ReturnType<typeof useSetAgentMcpToolAccess>;
  onSaved: () => void;
}) {
  const { t } = useT("agents");
  const modeGroup = useId();
  const [mode, setMode] = useState(server.allowed_tools == null ? "all" : "selected");
  const [selected, setSelected] = useState(() => new Set(server.allowed_tools ?? []));
  const [savedNames] = useState(() => server.allowed_tools ?? []);
  const [runtimeChoice, setRuntimeChoice] = useState("");
  const [saveError, setSaveError] = useState(false);
  const probe = useStartAgentMcpProbe(agentId, server.id);
  const runtimes = useQuery(runtimeListOptions(workspaceId, "me"));
  const eligible = (runtimes.data ?? []).filter(
    (candidate) => candidate.status === "online" && supportsMcpToolAccess(candidate),
  );
  const runtimeId = eligible.some((candidate) => candidate.id === runtimeChoice)
    ? runtimeChoice
    : eligible.find((candidate) => candidate.id === runtime?.id)?.id ?? eligible[0]?.id ?? "";
  const requestId = probe.data?.id;
  const result = useQuery({
    queryKey: ["workspaces", workspaceId, "agents", agentId, "mcp-probes", server.id, requestId],
    queryFn: ({ signal }) => api.getAgentMcpProbe(agentId, server.id, requestId!, signal),
    enabled: !!requestId,
    staleTime: Infinity,
    retry: false,
    refetchInterval: (query) => {
      const status = query.state.data?.status;
      return status === "pending" || status === "running" ? 700 : false;
    },
  });

  const probeStatus = result.data?.status;
  const loading = probe.isPending || (!!requestId && result.isPending) ||
    probeStatus === "pending" || probeStatus === "running";
  const discoveryFailed = probe.isError || (probe.isSuccess && !probe.data) ||
    result.isError || (result.isSuccess && !result.data) ||
    probeStatus === "failed" || probeStatus === "timeout";
  const discovered = probeStatus === "completed" ? result.data?.tools : undefined;
  const options = Array.from(new Set([...(discovered ?? []), ...selected, ...savedNames])).sort();
  const runtimeSupported = supportsMcpToolAccess(runtime);
  const canSelect = server.transport === "http" && runtimeSupported;

  async function handleSave() {
    setSaveError(false);
    try {
      await save.mutateAsync({ serverId: server.id, tools: mode === "all" ? null : Array.from(selected).sort() });
      onSaved();
    } catch {
      setSaveError(true);
    }
  }

  return (
    <>
      <DialogHeader>
        <DialogTitle className="break-all">
          {t(($) => $.tab_body.mcp_config.tool_access.title, { name: server.name })}
        </DialogTitle>
        <DialogDescription>
          {t(($) => $.tab_body.mcp_config.tool_access.scope)}
        </DialogDescription>
      </DialogHeader>
      <div className="min-h-0 space-y-4 overflow-y-auto">
        <fieldset disabled={save.isPending} className="space-y-2">
          <legend className="mb-2 text-label font-medium">
            {t(($) => $.tab_body.mcp_config.tool_access.mode)}
          </legend>
          <label className="flex items-center gap-2 text-body">
            <input type="radio" name={modeGroup} checked={mode === "all"} onChange={() => setMode("all")} />
            {t(($) => $.tab_body.mcp_config.tool_access.all)}
          </label>
          <label className="flex items-center gap-2 text-body">
            <input type="radio" name={modeGroup} checked={mode === "selected"} onChange={() => setMode("selected")} disabled={!canSelect && mode !== "selected"} />
            {t(($) => $.tab_body.mcp_config.tool_access.selected)}
          </label>
        </fieldset>
        {!canSelect && <p className="text-caption text-muted-foreground">
          {t(($) => $.tab_body.mcp_config.tool_access.requires_codex)}
        </p>}
        {mode === "selected" && (
          <div className="space-y-3">
            {server.transport === "http" && (
              <>
                <label className="block space-y-1.5 text-label">
                  <span>{t(($) => $.tab_body.mcp_config.tool_access.discovery_runtime)}</span>
                  <select
                    className="h-8 w-full rounded-md border border-input bg-background px-2 text-body focus-visible:outline-ring"
                    value={runtimeId}
                    onChange={(event) => {
                      setRuntimeChoice(event.target.value);
                      probe.reset();
                    }}
                    disabled={eligible.length === 0 || loading}
                  >
                    {eligible.length === 0 && <option value="">
                      {runtimes.isPending
                        ? t(($) => $.tab_body.mcp_config.tool_access.loading)
                        : t(($) => $.tab_body.mcp_config.tool_access.no_runtime)}
                    </option>}
                    {eligible.map((candidate) => (
                      <option key={candidate.id} value={candidate.id}>{runtimeDisplayLabel(candidate)}</option>
                    ))}
                  </select>
                </label>
                {runtimes.isError && (
                  <Button variant="outline" size="sm" onClick={() => void runtimes.refetch()}>
                    {t(($) => $.tab_body.mcp_config.tool_access.retry_runtimes)}
                  </Button>
                )}
                <Button
                  variant="outline" size="sm" disabled={!runtimeId || loading} aria-busy={loading}
                  onClick={() => probe.mutate(runtimeId)}
                >
                  {loading && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
                  {discoveryFailed
                    ? t(($) => $.tab_body.mcp_config.tool_access.retry)
                    : discovered
                      ? t(($) => $.tab_body.mcp_config.tool_access.refresh)
                      : t(($) => $.tab_body.mcp_config.tool_access.load)}
                </Button>
              </>
            )}
            {discoveryFailed && <p role="alert" className="text-caption text-destructive">
              {result.data?.error_code === "unauthorized"
                ? t(($) => $.tab_body.mcp_config.tool_access.unauthorized)
                : probe.error instanceof ApiError && probe.error.status === 409
                  ? t(($) => $.tab_body.mcp_config.tool_access.requires_codex)
                  : t(($) => $.tab_body.mcp_config.tool_access.load_failed)}
            </p>}
            {discovered?.length === 0 && (
              <p role="status" className="text-caption text-muted-foreground">
                {t(($) => $.tab_body.mcp_config.tool_access.no_tools)}
              </p>
            )}
            {options.length > 0 && (
              <ul className="max-h-48 space-y-1 overflow-y-auto rounded-md border p-2">
                {options.map((name) => (
                  <li key={name}>
                    <label className="flex items-center gap-2 px-2 py-1.5">
                      <Checkbox checked={selected.has(name)} disabled={save.isPending} onCheckedChange={(checked) => {
                        setSelected((previous) => {
                          const next = new Set(previous);
                          if (checked) next.add(name);
                          else next.delete(name);
                          return next;
                        });
                      }} />
                      <span className="min-w-0 break-all font-mono text-caption">{name}</span>
                      {discovered && !discovered.includes(name) && (
                        <span className="ml-auto text-caption text-muted-foreground">
                          {t(($) => $.tab_body.mcp_config.tool_access.missing)}
                        </span>
                      )}
                    </label>
                  </li>
                ))}
              </ul>
            )}
            {selected.size === 0 && <p className="text-caption text-muted-foreground">
              {t(($) => $.tab_body.mcp_config.tool_access.deny_all)}
            </p>}
          </div>
        )}
        {saveError && <p role="alert" className="text-caption text-destructive">
          {t(($) => $.tab_body.mcp_config.tool_access.save_failed)}
        </p>}
      </div>
      <DialogFooter>
        <DialogClose render={<Button variant="outline" disabled={save.isPending} />}>
          {t(($) => $.tab_body.mcp_config.tool_access.cancel)}
        </DialogClose>
        <Button disabled={save.isPending || (mode === "selected" && !canSelect)} aria-busy={save.isPending} onClick={() => void handleSave()}>
          {save.isPending && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
          {t(($) => $.tab_body.mcp_config.tool_access.save)}
        </Button>
      </DialogFooter>
    </>
  );
}
