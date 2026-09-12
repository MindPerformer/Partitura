<!-- pages/workspaces/[id]/settings.vue — Workspace Settings
//
// 引入动机：design/04-WEB-API.md §页面 要求 Workspace Settings 页面。
// 按 RBAC 控制设置操作的可见性（admin+）。
-->
<script setup lang="ts">
import type { ApiError } from "~/types/api";

definePageMeta({
  middleware: ["auth"],
});

const { t } = useI18n();
const route = useRoute();
const workspaceId = computed(() => route.params.id as string);

const { workspace, canSettings, isOwner, reload } =
  useWorkspaceContext(workspaceId);
const { isSystemAdmin } = useAuth();
const { workspaceStatusLabel } = useEnumLabels();
const toast = useToast();

const form = reactive({
  display_name: "",
  description: "",
  revision_retention_days: 30,
  revision_max_count: 100,
  max_document_size_bytes: 1048576,
});

const loading = ref(false);
const saving = ref(false);
const error = ref<string | null>(null);
const success = ref(false);
let successTimer: ReturnType<typeof setTimeout> | undefined;

// 归档工作区确认（UModal + 输名二次确认，取代原生 confirm 的高危操作确认）
const showArchiveModal = ref(false);
const archiveConfirmName = ref("");
const archiveLoading = ref(false);

watch(
  workspace,
  (ws) => {
    if (ws) {
      form.display_name = ws.display_name;
      form.description = ws.description;
      form.revision_retention_days = ws.revision_retention_days;
      form.revision_max_count = ws.revision_max_count;
      form.max_document_size_bytes = ws.max_document_size_bytes;
    }
  },
  { immediate: true },
);

async function handleSave() {
  if (!form.display_name) {
    error.value = t("workspace.nameAndDisplayNameRequired");
    return;
  }
  saving.value = true;
  error.value = null;
  success.value = false;

  try {
    const api = useWorkspaceApi();
    await api.update(workspaceId.value, {
      display_name: form.display_name,
      description: form.description,
      revision_retention_days: form.revision_retention_days,
      revision_max_count: form.revision_max_count,
      max_document_size_bytes: form.max_document_size_bytes,
    });
    success.value = true;
    // 成功横幅自动消失，避免常驻占用视觉焦点。
    if (successTimer) clearTimeout(successTimer);
    successTimer = setTimeout(() => {
      success.value = false;
    }, 4000);
    toast.add({ title: t("workspace.settingsSaved"), color: "success" });
    await reload();
  } catch (err) {
    const apiErr = err as ApiError;
    error.value = apiErr.error || t("workspace.createFailed");
  } finally {
    saving.value = false;
  }
}

function promptArchive() {
  archiveConfirmName.value = "";
  showArchiveModal.value = true;
}

// 高危操作：必须输入 workspace 显示名才能确认归档。
const archiveNameMatches = computed(
  () =>
    !!workspace.value &&
    archiveConfirmName.value.trim() === workspace.value.display_name,
);

async function confirmArchive() {
  if (!archiveNameMatches.value) return;
  archiveLoading.value = true;
  try {
    const api = useWorkspaceApi();
    await api.archive(workspaceId.value);
    showArchiveModal.value = false;
    toast.add({ title: t("workspace.workspaceArchived"), color: "success" });
    navigateTo("/");
  } catch (err) {
    const apiErr = err as ApiError;
    error.value = apiErr.error || t("workspace.archiveFailed");
  } finally {
    archiveLoading.value = false;
  }
}

onBeforeUnmount(() => {
  if (successTimer) clearTimeout(successTimer);
});

useHead({
  title: () => t("workspace.workspaceSettings") + " · " + t("common.appName"),
});
</script>

<template>
  <WorkspaceLayout>
    <div class="max-w-2xl mx-auto px-4 py-8">
      <h1 class="text-2xl font-bold text-highlighted mb-6">
        {{ t("workspace.workspaceSettings") }}
      </h1>

      <div v-if="!canSettings" class="text-center py-12">
        <UIcon name="i-lucide-lock" class="w-12 h-12 text-muted mx-auto mb-3" />
        <p class="text-muted">{{ t("workspace.noPermissionSettings") }}</p>
      </div>

      <div v-else-if="workspace">
        <ErrorDisplay v-if="error" :message="error" class="mb-4" />

        <div
          v-if="success"
          class="rounded-lg border border-success/20 bg-success/10 p-3 mb-4"
        >
          <p class="text-sm text-success">{{ t("workspace.settingsSaved") }}</p>
        </div>

        <UCard>
          <form @submit.prevent="handleSave" class="space-y-4">
            <UFormField :label="t('workspace.displayName')" name="display_name">
              <UInput v-model="form.display_name" class="w-full" />
            </UFormField>
            <UFormField :label="t('workspace.description')" name="description">
              <UTextarea v-model="form.description" class="w-full" :rows="4" />
            </UFormField>

            <UFormField
              :label="t('workspace.revisionRetentionDays')"
              name="revision_retention_days"
            >
              <UInput
                v-model.number="form.revision_retention_days"
                type="number"
                class="w-full"
              />
            </UFormField>
            <UFormField
              :label="t('workspace.revisionMaxCount')"
              name="revision_max_count"
            >
              <UInput
                v-model.number="form.revision_max_count"
                type="number"
                class="w-full"
              />
            </UFormField>
            <UFormField
              :label="t('workspace.maxDocumentSize')"
              name="max_document_size_bytes"
            >
              <UInput
                v-model.number="form.max_document_size_bytes"
                type="number"
                class="w-full"
              />
            </UFormField>

            <div
              class="text-sm text-muted space-y-1 pt-2 border-t border-default"
            >
              <p>
                <span class="font-medium">{{ t("workspace.name") }}:</span>
                {{ workspace.name }}
              </p>
              <p>
                <span class="font-medium">{{ t("workspace.status") }}:</span>
                {{ workspaceStatusLabel(workspace.status) }}
              </p>
              <p>
                <span class="font-medium">{{ t("workspace.retention") }}:</span>
                {{
                  t("workspace.retentionFormat", {
                    days: workspace.revision_retention_days,
                    count: workspace.revision_max_count,
                  })
                }}
              </p>
              <p>
                <span class="font-medium"
                  >{{ t("workspace.maxDocumentSize") }}:</span
                >
                {{
                  (workspace.max_document_size_bytes / 1024 / 1024).toFixed(1)
                }}
                MB
              </p>
            </div>

            <div class="flex justify-end">
              <UButton type="submit" :loading="saving" :disabled="saving">{{
                t("workspace.saveSettings")
              }}</UButton>
            </div>
          </form>
        </UCard>

        <UCard v-if="isSystemAdmin" class="mt-6">
          <h2 class="font-semibold mb-2">{{ t("admin.tuning") }}</h2>
          <p class="text-sm text-muted mb-3">
            {{ t("admin.workspaceTuningHint") }}
          </p>
          <UButton
            :to="{
              path: '/admin/tuning',
              query: { workspace_id: workspaceId },
            }"
            icon="i-lucide-sliders-horizontal"
            variant="outline"
          >
            {{ t("admin.openWorkspaceTuning") }}
          </UButton>
        </UCard>

        <!-- Danger zone -->
        <UCard v-if="isOwner || canSettings" class="mt-6 border-error/20">
          <template #header>
            <h2 class="font-semibold text-error">
              {{ t("workspace.dangerZone") }}
            </h2>
          </template>
          <div class="flex items-center justify-between">
            <div>
              <p class="text-sm font-medium">
                {{ t("workspace.archiveWorkspace") }}
              </p>
              <p class="text-xs text-muted">
                {{ t("workspace.archiveWorkspaceDesc") }}
              </p>
            </div>
            <UButton color="error" variant="outline" @click="promptArchive">{{
              t("common.archive")
            }}</UButton>
          </div>
        </UCard>
      </div>
    </div>

    <!-- 归档工作区确认（需输入 workspace 名） -->
    <UModal v-model:open="showArchiveModal" :dismissible="!archiveLoading">
      <template #content>
        <div class="p-6">
          <div class="flex items-start gap-3 mb-4">
            <UIcon
              name="i-lucide-triangle-alert"
              class="w-6 h-6 text-error flex-shrink-0"
            />
            <div>
              <h3 class="text-lg font-semibold text-highlighted">
                {{ t("workspace.archiveWorkspace") }}
              </h3>
              <p class="text-sm text-muted mt-1">
                {{ t("workspace.archiveConfirm") }}
              </p>
            </div>
          </div>
          <UFormField
            :label="
              t('workspace.archiveConfirmLabel', {
                name: workspace?.display_name ?? '',
              })
            "
            name="archive_name"
            class="mt-4"
          >
            <UInput
              v-model="archiveConfirmName"
              :placeholder="workspace?.display_name"
              class="w-full"
              autocomplete="off"
            />
          </UFormField>
          <div class="flex justify-end gap-2 mt-6">
            <UButton
              color="neutral"
              variant="ghost"
              @click="showArchiveModal = false"
              >{{ t("common.cancel") }}</UButton
            >
            <UButton
              color="error"
              :loading="archiveLoading"
              :disabled="!archiveNameMatches"
              @click="confirmArchive"
              >{{ t("common.archive") }}</UButton
            >
          </div>
        </div>
      </template>
    </UModal>
  </WorkspaceLayout>
</template>
