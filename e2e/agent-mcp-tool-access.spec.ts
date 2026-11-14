import { expect, test } from "@playwright/test";
import { TestApiClient } from "./fixtures";

test("keeps saved MCP tool selections through discovery failure and retry", async ({ page }, info) => {
  const api = new TestApiClient();
  const id = crypto.randomUUID();
  const login = await api.login(`mcp-tools-${id}@multica.ai`, "MCP Tools Tester");
  const workspace = await api.ensureWorkspace("MCP Tools", `mcp-tools-${id}`);
  await api.markUserOnboarded();

  try {
    const agentId = crypto.randomUUID();
    const runtimeId = crypto.randomUUID();
    const serverId = crypto.randomUUID();
    const ownerId = login.user.id;
    await page.addInitScript((token) => {
      if (token) localStorage.setItem("multica_token", token);
      localStorage.setItem("multica:chat:isOpen", "false");
    }, api.getToken());

    const agent = {
      id: agentId, workspace_id: workspace.id, runtime_id: runtimeId,
      name: "Researcher", owner_id: ownerId, visibility: "workspace", status: "idle",
      runtime_mode: "local", runtime_config: {}, custom_args: [], skills: [],
    };
    const runtime = {
      id: runtimeId, workspace_id: workspace.id, owner_id: ownerId, name: "Hermes",
      provider: "hermes", status: "online", runtime_mode: "local",
      metadata: { capabilities: ["hermes-workspace-mcp-tools-v1"] },
    };
    let server = {
      id: serverId, workspace_id: workspace.id, name: "workspace-issues",
      enabled: true, transport: "http", allowed_tools: ["search_issues", "saved_tool"],
    };
    await page.route((url) => url.pathname === "/api/agents", (route) => route.fulfill({ json: [agent] }));
    await page.route((url) => url.pathname === `/api/agents/${agentId}`, (route) => route.fulfill({ json: agent }));
    await page.route((url) => url.pathname === "/api/runtimes", (route) => route.fulfill({ json: [runtime] }));
    await page.route(`**/api/agents/${agentId}/mcp-servers`, (route) => route.fulfill({ json: [server] }));
    await page.route(`**/api/workspaces/${workspace.id}/mcp-servers`, (route) => route.fulfill({ json: [server] }));
    await page.route(`**/api/agents/${agentId}/mcp-servers/${serverId}/tool-access`, async (route) => {
      const policy = route.request().postDataJSON();
      expect(policy).toEqual({ mode: "selected", tools: ["saved_tool", "search_issues"] });
      server = { ...server, allowed_tools: policy.tools };
      await route.fulfill({ json: [server] });
    });

    let probes = 0;
    await page.route(`**/mcp-servers/${serverId}/probe`, (route) => {
      expect(route.request().postData()).toBeNull();
      probes++;
      return route.fulfill({ json: { id: String(probes), status: "pending", tools: [] } });
    });
    await page.route(`**/mcp-servers/${serverId}/probes/*`, (route) => route.fulfill({
      json: probes === 1
        ? { id: "1", status: "failed", error_code: "handshake", tools: [] }
        : { id: "2", status: "completed", tools: ["search_issues", "get_issue", "create_issue", "update_issue"], read_only_tools: ["search_issues", "get_issue"] },
    }));

    await page.goto(`/${workspace.slug}/agents/${agentId}?view=mcp_config`, { waitUntil: "domcontentloaded" });
    await page.getByRole("button", { name: "Tool access", exact: true }).click();
    const dialog = page.getByRole("dialog", { name: "Tool access for workspace-issues" });
    await expect(dialog.getByRole("combobox")).toHaveCount(0);
    await dialog.getByRole("button", { name: "Load tools" }).click();
    await expect(dialog.getByRole("alert")).toContainText("Could not load the tool list");
    await dialog.getByRole("button", { name: "Retry discovery" }).click();
    const readGroup = dialog.getByRole("group", { name: /^Read \(\d+\)$/ });
    const readTool = readGroup.getByRole("checkbox", { name: "search_issues" });
    const otherReadTool = readGroup.getByRole("checkbox", { name: "get_issue" });
    const readSwitch = readGroup.getByRole("switch", { name: "Read", exact: true });
    await expect(readTool).toBeChecked();
    await expect(readGroup).toHaveAccessibleName("Read (1)");
    await expect(readSwitch).not.toBeChecked();
    await readSwitch.click();
    await expect(readSwitch).toBeChecked();
    await expect(otherReadTool).toBeChecked();
    await expect(readGroup).toHaveAccessibleName("Read (2)");
    await readSwitch.click();
    await expect(readGroup).toHaveAccessibleName("Read (0)");
    await expect(otherReadTool).not.toBeChecked();
    await expect(readTool).not.toBeChecked();
    await readTool.check();
    await expect(readGroup).toHaveAccessibleName("Read (1)");
    const writeGroup = dialog.getByRole("group", { name: "Read / Write", exact: true });
    const writeSwitch = writeGroup.getByRole("switch", { name: "Read / Write" });
    const writeTool = writeGroup.getByRole("checkbox", { name: "create_issue" });
    const otherWriteTool = writeGroup.getByRole("checkbox", { name: "update_issue" });
    await writeSwitch.click();
    await expect(writeTool).toBeChecked();
    await expect(otherWriteTool).toBeChecked();
    await expect(readGroup).toHaveAccessibleName("Read (1)");
    await writeSwitch.click();
    await expect(writeTool).not.toBeChecked();
    await expect(otherWriteTool).not.toBeChecked();
    await expect(dialog.getByRole("checkbox", { name: /saved_tool/ })).toBeChecked();
    await expect(dialog.getByText("Unavailable on this runtime")).toBeVisible();
    await info.attach("MCP tool access", {
      body: await page.screenshot({ animations: "disabled" }), contentType: "image/png",
    });
    await dialog.getByRole("button", { name: "Save", exact: true }).click();
    await expect(dialog).toHaveCount(0);
  } finally {
    await api.deleteWorkspace();
  }
});
