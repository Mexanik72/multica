"use client";

import { useId, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, ApiError } from "@multica/core/api";
import { runtimeDisplayLabel } from "@multica/core/runtimes";
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
  if (runtime?.runtime_mode !== "local" || runtime.profile_id ||
      !Array.isArray(runtime.metadata?.capabilities)) {
    return false;
  }
  switch (runtime.provider) {
    case "codex":
      return runtime.metadata.capabilities.includes("workspace-mcp-tools-v1");
    case "hermes":
      return runtime.metadata.capabilities.includes("hermes-workspace-mcp-tools-v1");
    default:
      return false;
  }
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
        {open && <McpToolAccessForm key={props.runtime?.id} {...props} save={save} onSaved={() => setOpen(false)} />}
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
  const [saveError, setSaveError] = useState(false);
  const probe = useStartAgentMcpProbe(agentId, server.id);
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
  const available = new Set(discovered);
  const readOnly = new Set(result.data?.read_only_tools);
  const options = Array.from(new Set([...available, ...selected, ...savedNames])).sort();
  const groups = [
    { label: t(($) => $.tab_body.mcp_config.tool_access.read), showCount: true, names: options.filter((name) => available.has(name) && readOnly.has(name)) },
    { label: t(($) => $.tab_body.mcp_config.tool_access.read_write), names: options.filter((name) => available.has(name) && !readOnly.has(name)) },
    { label: t(($) => $.tab_body.mcp_config.tool_access.saved), names: options.filter((name) => !available.has(name)) },
  ];
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
          {t(($) => $.tab_body.mcp_config.tool_access.requires_runtime)}
        </p>}
        {mode === "selected" && (
          <div className="space-y-3">
            {runtime && server.transport === "http" && (
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="min-w-0 break-words text-caption text-muted-foreground">
                  {t(($) => $.tab_body.mcp_config.tool_access.agent_runtime, { runtime: runtimeDisplayLabel(runtime) })}
                </p>
                <Button
                  variant="outline" size="sm" disabled={!canSelect || loading} aria-busy={loading}
                  onClick={() => probe.mutate()}
                >
                  {loading && <Loader2 className="animate-spin motion-reduce:animate-none" aria-hidden="true" />}
                  {discoveryFailed
                    ? t(($) => $.tab_body.mcp_config.tool_access.retry)
                    : discovered
                      ? t(($) => $.tab_body.mcp_config.tool_access.refresh)
                      : t(($) => $.tab_body.mcp_config.tool_access.load)}
                </Button>
              </div>
            )}
            {discoveryFailed && <p role="alert" className="text-caption text-destructive">
              {result.data?.error_code === "unauthorized"
                ? t(($) => $.tab_body.mcp_config.tool_access.unauthorized)
                : probe.error instanceof ApiError && probe.error.status === 503
                  ? t(($) => $.tab_body.mcp_config.tool_access.runtime_offline)
                  : t(($) => $.tab_body.mcp_config.tool_access.load_failed)}
            </p>}
            {discovered?.length === 0 && (
              <p role="status" className="text-caption text-muted-foreground">
                {t(($) => $.tab_body.mcp_config.tool_access.no_tools)}
              </p>
            )}
            {options.length > 0 && (
              <div className="max-h-56 space-y-3 overflow-y-auto rounded-md border p-2">
                {groups.filter(({ names }) => names.length > 0).map(({ label, names, showCount }) => (
                  <fieldset key={label} className="space-y-1">
                    <legend className="px-2 text-label font-medium">
                      {label}
                      {showCount && (
                        <span className="text-muted-foreground tabular-nums">
                          {" ("}{names.filter((name) => selected.has(name)).length}{")"}
                        </span>
                      )}
                    </legend>
                    {names.map((name) => (
                      <label key={name} className="flex items-center gap-2 px-2 py-1.5">
                        <Checkbox checked={selected.has(name)} disabled={save.isPending} onCheckedChange={(checked) => {
                          setSelected((previous) => {
                            const next = new Set(previous);
                            if (checked) next.add(name);
                            else next.delete(name);
                            return next;
                          });
                        }} />
                        <span className="min-w-0 break-all font-mono text-caption">{name}</span>
                        {discovered && !available.has(name) && (
                          <span className="ml-auto text-caption text-muted-foreground">
                            {t(($) => $.tab_body.mcp_config.tool_access.missing)}
                          </span>
                        )}
                      </label>
                    ))}
                  </fieldset>
                ))}
              </div>
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
