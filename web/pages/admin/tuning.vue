<script setup lang="ts">
import type {
  ApiError,
  TuningConfig,
  TuningParameters,
  TuningRecommendation,
  TuningScope,
  Workspace,
} from "~/types/api";

definePageMeta({ middleware: ["auth", "admin"] });

const { t } = useI18n();
const toast = useToast();
const scope = ref<TuningScope>("global");
const workspaceId = ref("");
const workspaces = ref<Workspace[]>([]);
const config = ref<TuningConfig | null>(null);
const parameters = reactive<TuningParameters>({});
const recommendations = ref<TuningRecommendation[]>([]);
const loading = ref(false);
const saving = ref(false);
const validating = ref(false);
const error = ref<string | null>(null);
const validationQueries = ref("");
const validation = ref<import("~/types/api").TuningValidationResponse | null>(
  null,
);
const pendingAction = ref<"publish" | "rollback" | "release" | null>(null);

const groups = computed(() => {
  const definitions = config.value?.parameters ?? [];
  const titles: Record<string, string> = {
    retrieval: t("admin.tuningRetrieval"),
    chunk: t("admin.tuningChunk"),
    budget: t("admin.tuningBudget"),
    other: t("admin.tuningOther"),
  };
  return [...new Set(definitions.map((item) => item.group))]
    .map((group) => ({
      key: group,
      title: titles[group] ?? group,
      items: definitions.filter((item) => item.group === group),
    }))
    .filter((group) => group.items.length > 0);
});
const workspaceOptions = computed(() =>
  workspaces.value.map((ws) => ({ label: ws.display_name, value: ws.id })),
);
const canLoad = computed(() => scope.value === "global" || !!workspaceId.value);

function profileParameters(profile: Record<string, unknown>): TuningParameters {
  const keys = [
    "title_boost",
    "heading_boost",
    "path_boost",
    "tags_boost",
    "body_boost",
    "lexical_top_k",
    "vector_top_k",
    "rrf_k",
    "reranker_candidate_count",
    "reranker_final_count",
    "max_chunks_per_document",
    "merge_adjacent_chunks",
    "max_p95_latency_ms",
    "max_reranker_cost_per_query",
  ];
  return Object.fromEntries(
    keys
      .filter((key) => profile[key] !== undefined)
      .map((key) => [key, profile[key]]),
  ) as TuningParameters;
}
function applyConfig(next: TuningConfig) {
  config.value = next;
  for (const key of Object.keys(parameters)) delete parameters[key];
  Object.assign(
    parameters,
    next.draft ??
      next.published ??
      profileParameters(
        next.effective_config as unknown as Record<string, unknown>,
      ),
  );
}
async function loadWorkspaces() {
  try {
    workspaces.value = (
      await useAdminApi().listAllWorkspaces({ limit: 100 })
    ).workspaces;
  } catch (err) {
    error.value = (err as ApiError).error || t("admin.loadWorkspacesFailed");
  }
}
async function load() {
  if (!canLoad.value) return;
  loading.value = true;
  error.value = null;
  validation.value = null;
  try {
    const api = useTuningApi();
    applyConfig(await api.get(scope.value, workspaceId.value || undefined));
    recommendations.value = (
      await api.recommendations(scope.value, workspaceId.value || undefined)
    ).recommendations;
  } catch (err) {
    error.value = (err as ApiError).error || t("admin.tuningLoadFailed");
  } finally {
    loading.value = false;
  }
}
async function validate() {
  if (!canLoad.value) return;
  validating.value = true;
  error.value = null;
  try {
    validation.value = await useTuningApi().validate(
      scope.value,
      {
        parameters: { ...parameters },
        queries: validationQueries.value
          .split("\n")
          .map((query) => query.trim())
          .filter(Boolean)
          .slice(0, 5),
        ...(scope.value === "workspace" && workspaceId.value
          ? { workspace_id: workspaceId.value }
          : {}),
      },
      workspaceId.value || undefined,
    );
  } catch (err) {
    error.value = (err as ApiError).error || t("admin.tuningValidateFailed");
  } finally {
    validating.value = false;
  }
}
async function save() {
  if (!canLoad.value) return;
  saving.value = true;
  error.value = null;
  try {
    applyConfig(
      await useTuningApi().updateDraft(
        scope.value,
        {
          expected_revision: config.value?.revision ?? 0,
          parameters: { ...parameters },
        },
        workspaceId.value || undefined,
      ),
    );
    toast.add({ title: t("admin.tuningSaved"), color: "success" });
  } catch (err) {
    error.value = (err as ApiError).error || t("admin.tuningSaveFailed");
  } finally {
    saving.value = false;
  }
}
async function applyRecommendation(item: TuningRecommendation) {
  Object.assign(parameters, item.parameters);
  item.status = "applied";
  await save();
}
async function confirmAction() {
  if (!pendingAction.value) return;
  const action = pendingAction.value;
  pendingAction.value = null;
  try {
    const api = useTuningApi();
    const expected_revision = config.value?.revision ?? 0;
    if (action === "publish")
      await api.publish(
        scope.value,
        { expected_revision },
        workspaceId.value || undefined,
      );
    else if (action === "rollback") {
      const target_revision = config.value?.history.at(-1)?.revision;
      if (target_revision === undefined)
        throw new Error("no rollback target available");
      await api.rollback(
        scope.value,
        { expected_revision, target_revision },
        workspaceId.value || undefined,
      );
    } else {
      if (scope.value !== "workspace")
        throw new Error("global tuning has no override");
      await api.releaseOverride(
        scope.value,
        { expected_revision },
        workspaceId.value,
      );
    }
    toast.add({ title: t("admin.tuningActionSucceeded"), color: "success" });
    await load();
  } catch (err) {
    error.value = (err as ApiError).error || t("admin.tuningActionFailed");
  }
}
watch(scope, () => {
  if (scope.value === "global") void load();
});
watch(workspaceId, () => {
  if (scope.value === "workspace") void load();
});
onMounted(async () => {
  await loadWorkspaces();
  await load();
});
useHead({ title: () => t("admin.tuning") + " · " + t("common.appName") });
</script>

<template>
  <div class="max-w-6xl mx-auto px-4 py-8">
    <div class="flex flex-wrap items-center justify-between gap-3 mb-6">
      <div>
        <h1 class="text-2xl font-bold text-highlighted">
          {{ t("admin.tuning") }}
        </h1>
        <p class="text-sm text-muted mt-1">{{ t("admin.tuningDesc") }}</p>
      </div>
      <div class="flex gap-2">
        <UButton
          :variant="scope === 'global' ? 'solid' : 'outline'"
          @click="scope = 'global'"
          >{{ t("admin.globalScope") }}</UButton
        ><UButton
          :variant="scope === 'workspace' ? 'solid' : 'outline'"
          @click="scope = 'workspace'"
          >{{ t("admin.workspaceScope") }}</UButton
        >
      </div>
    </div>
    <ErrorDisplay v-if="error" :message="error" class="mb-4" />
    <UCard class="mb-6"
      ><div v-if="scope === 'workspace'" class="max-w-md">
        <UFormField :label="t('admin.selectWorkspace')"
          ><USelect
            v-model="workspaceId"
            :items="workspaceOptions"
            value-key="value"
            class="w-full"
        /></UFormField>
      </div>
      <p v-else class="text-sm text-muted">
        {{ t("admin.globalScopeHint") }}
      </p></UCard
    >
    <div v-if="loading" class="flex justify-center py-12" role="status">
      <UIcon
        name="i-lucide-loader-circle"
        class="w-8 h-8 animate-spin text-muted"
      />
    </div>
    <template v-else-if="canLoad && config">
      <UAlert
        v-if="config.capabilities"
        color="neutral"
        variant="soft"
        class="mb-4"
        :title="t('admin.tuningCapabilities')"
      >
        {{
          config.capabilities.can_edit === false
            ? t("admin.tuningReadOnly")
            : t("admin.tuningEditable")
        }}
        <span v-if="config.capabilities.can_publish === false">
          · {{ t("admin.tuningPublishUnavailable") }}</span
        >
      </UAlert>
      <div class="flex flex-wrap justify-end gap-2 mb-4">
        <UButton variant="outline" :loading="validating" @click="validate">{{
          t("admin.quickValidate")
        }}</UButton
        ><UButton :loading="saving" @click="save">{{
          t("common.save")
        }}</UButton
        ><UButton color="primary" @click="pendingAction = 'publish'">{{
          t("admin.publishTuning")
        }}</UButton
        ><UButton
          color="warning"
          variant="outline"
          @click="pendingAction = 'rollback'"
          >{{ t("admin.rollbackTuning") }}</UButton
        ><UButton
          v-if="scope === 'workspace'"
          color="error"
          variant="outline"
          @click="pendingAction = 'release'"
          >{{ t("admin.releaseOverride") }}</UButton
        >
      </div>
      <UCard class="mb-4">
        <UFormField
          :label="t('admin.tuningValidationQueries')"
          :hint="t('admin.tuningValidationQueriesHint')"
        >
          <UTextarea
            v-model="validationQueries"
            :rows="3"
            :placeholder="t('admin.tuningValidationQueriesPlaceholder')"
            class="w-full"
          />
        </UFormField>
      </UCard>
      <UAlert
        v-if="validation"
        :color="validation.valid ? 'success' : 'error'"
        :title="
          validation.valid ? t('admin.tuningValid') : t('admin.tuningInvalid')
        "
        class="mb-4"
        ><ul class="list-disc pl-5">
          <li v-for="item in validation.checks" :key="item.code">
            {{ item.message }}
          </li>
        </ul></UAlert
      >
      <UCard v-if="validation?.results.length" class="mb-4">
        <template #header
          ><h2 class="font-semibold">
            {{ t("admin.tuningValidationResults") }}
          </h2></template
        >
        <div class="overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-muted">
                <th class="p-2">{{ t("admin.query") }}</th>
                <th class="p-2">{{ t("admin.tuningOverlap") }}</th>
                <th class="p-2">{{ t("admin.tuningBaseline") }}</th>
                <th class="p-2">{{ t("admin.tuningCandidate") }}</th>
              </tr>
            </thead>
            <tbody>
              <tr
                v-for="result in validation.results"
                :key="result.query"
                class="border-t border-default"
              >
                <td class="p-2">{{ result.query }}</td>
                <td class="p-2">{{ result.overlap_ratio }}</td>
                <td class="p-2">{{ result.baseline.latency_ms ?? "—" }} ms</td>
                <td class="p-2">{{ result.candidate.latency_ms ?? "—" }} ms</td>
              </tr>
            </tbody>
          </table>
        </div>
      </UCard>
      <div class="grid gap-4 lg:grid-cols-2">
        <UCard v-for="group in groups" :key="group.key"
          ><template #header
            ><h2 class="font-semibold">{{ group.title }}</h2></template
          >
          <div class="space-y-3">
            <UFormField
              v-for="item in group.items"
              :key="item.key"
              :label="item.label || item.key"
              :hint="item.description"
              ><USwitch
                v-if="item.type === 'bool'"
                :model-value="parameters[item.key] === true"
                @update:model-value="parameters[item.key] = $event" />
              <UInput
                v-else
                :model-value="String(parameters[item.key] ?? '')"
                type="number"
                @update:model-value="
                  parameters[item.key] = $event === '' ? null : Number($event)
                "
                :min="item.min"
                :max="item.max"
                :step="item.type === 'float' ? 'any' : 1"
                class="w-full"
            /></UFormField></div
        ></UCard>
      </div>
      <UCard v-if="recommendations.length" class="mt-6"
        ><template #header
          ><h2 class="font-semibold">
            {{ t("admin.tuningRecommendations") }}
          </h2></template
        >
        <div class="space-y-3">
          <div
            v-for="item in recommendations"
            :key="item.id"
            class="flex items-center justify-between gap-3 border-b border-default pb-3 last:border-0"
          >
            <div>
              <p class="font-medium">{{ item.title }}</p>
              <p class="text-sm text-muted">{{ item.reason }}</p>
            </div>
            <UButton
              size="sm"
              :disabled="item.status === 'applied'"
              @click="applyRecommendation(item)"
              >{{
                item.status === "applied"
                  ? t("admin.applied")
                  : t("admin.applyRecommendation")
              }}</UButton
            >
          </div>
        </div></UCard
      >
    </template>
    <EmptyState
      v-else-if="!canLoad"
      icon="i-lucide-folder"
      :title="t('admin.selectWorkspace')"
    />
    <UModal
      :open="!!pendingAction"
      @update:open="
        (v) => {
          if (!v) pendingAction = null;
        }
      "
      ><template #content
        ><div class="p-6">
          <h2 class="text-lg font-semibold">
            {{ t("admin.tuningConfirmTitle") }}
          </h2>
          <p class="text-sm text-muted mt-2">
            {{ t("admin.tuningConfirmBody") }}
          </p>
          <div class="flex justify-end gap-2 mt-6">
            <UButton variant="ghost" @click="pendingAction = null">{{
              t("common.cancel")
            }}</UButton
            ><UButton color="primary" @click="confirmAction">{{
              t("common.confirm")
            }}</UButton>
          </div>
        </div></template
      ></UModal
    >
  </div>
</template>
