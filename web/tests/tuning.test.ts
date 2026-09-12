import { beforeEach, describe, expect, it } from "vitest";
import { createFetchMock } from "./setup";

const ctrl = createFetchMock();

describe("Tuning API", () => {
  beforeEach(() => ctrl.reset());

  it("loads global tuning configuration", async () => {
    ctrl.setResponse({
      scope: "global",
      revision: 3,
      parameters: [],
      capabilities: { can_edit: true, can_publish: true },
    });
    const { useTuningApi } = await import("~/composables/useApi");
    const result = await useTuningApi().get("global");
    expect(result.revision).toBe(3);
    expect(ctrl.mockFn.mock.calls[0]![0]).toContain("/admin/tuning");
  });

  it("saves workspace draft with optimistic revision", async () => {
    ctrl.setResponse({ scope: "workspace", workspace_id: "ws-1", revision: 4 });
    const { useTuningApi } = await import("~/composables/useApi");
    await useTuningApi().updateDraft(
      "workspace",
      { expected_revision: 3, parameters: { title_boost: 2 } },
      "ws-1",
    );
    const [url, options] = ctrl.mockFn.mock.calls[0]! as [
      string,
      { method: string; body: string },
    ];
    expect(url).toContain("/workspaces/ws-1/tuning/draft");
    expect(options.method).toBe("PUT");
    expect(JSON.parse(options.body)).toEqual({
      expected_revision: 3,
      parameters: { title_boost: 2 },
    });
  });

  it("uses recommendations endpoint and strict validate payload", async () => {
    ctrl.setResponse({ recommendations: [], automatic_publish: false });
    const { useTuningApi } = await import("~/composables/useApi");
    await useTuningApi().recommendations("global");
    expect(ctrl.mockFn.mock.calls[0]![0]).toContain(
      "/admin/tuning/recommendations",
    );
    ctrl.reset();
    ctrl.setResponse({
      valid: true,
      checks: [],
      effective_config: {},
      results: [],
      publish_gate: false,
    });
    await useTuningApi().validate("global", {
      parameters: { title_boost: 2 },
      queries: ["hello"],
    });
    const options = ctrl.mockFn.mock.calls[0]![1] as { body: string };
    expect(JSON.parse(options.body)).toEqual({
      parameters: { title_boost: 2 },
      queries: ["hello"],
    });
  });

  it("fails fast when workspace scope has no workspace id", async () => {
    const { useTuningApi } = await import("~/composables/useApi");
    expect(() => useTuningApi().get("workspace")).toThrow("workspaceId");
  });
});
