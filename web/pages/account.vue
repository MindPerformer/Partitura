<!-- pages/account.vue — 账户设置 -->
<script setup lang="ts">
import type { ApiError, CurrentUserResponse } from '~/types/api'

definePageMeta({
  middleware: ['auth']
})

const { t } = useI18n()
const auth = useAuth()
const { systemRoleLabel } = useEnumLabels()
const profile = ref<CurrentUserResponse | null>(null)
const loading = ref(false)
const loadError = ref<string | null>(null)

const emailForm = reactive({ current_password: '', email: '' })
const emailLoading = ref(false)
const emailError = ref<string | null>(null)
const emailSuccess = ref(false)

const passwordForm = reactive({ current_password: '', new_password: '', confirm_password: '' })
const passwordLoading = ref(false)
const passwordError = ref<string | null>(null)
const passwordSuccess = ref(false)

async function loadProfile() {
  loading.value = true
  loadError.value = null
  try {
    const current = await useAuthApi().me()
    profile.value = current
    emailForm.email = current.email
    auth.setCurrentUser(current)
  } catch (err) {
    const apiErr = err as ApiError
    loadError.value = apiErr.error || t('account.loadFailed')
    if (import.meta.dev) {
      console.error('[account] 加载当前用户资料失败', err)
    }
  } finally {
    loading.value = false
  }
}

async function updateEmail() {
  emailError.value = null
  emailSuccess.value = false
  const email = emailForm.email.trim()
  if (!email || !email.includes('@')) {
    emailError.value = t('account.emailInvalid')
    return
  }
  if (!emailForm.current_password) {
    emailError.value = t('account.currentPasswordRequired')
    return
  }

  emailLoading.value = true
  try {
    const updated = await useAuthApi().updateEmail({
      current_password: emailForm.current_password,
      email
    })
    profile.value = updated
    emailForm.email = updated.email
    emailForm.current_password = ''
    auth.setCurrentUser(updated)
    emailSuccess.value = true
  } catch (err) {
    const apiErr = err as ApiError
    emailError.value = apiErr.error || t('account.updateEmailFailed')
  } finally {
    emailLoading.value = false
  }
}

async function updatePassword() {
  passwordError.value = null
  passwordSuccess.value = false
  if (!passwordForm.current_password) {
    passwordError.value = t('account.currentPasswordRequired')
    return
  }
  if (passwordForm.new_password.length < 12) {
    passwordError.value = t('account.passwordTooShort')
    return
  }
  if (passwordForm.new_password !== passwordForm.confirm_password) {
    passwordError.value = t('account.passwordMismatch')
    return
  }

  passwordLoading.value = true
  try {
    await useAuthApi().updatePassword({
      current_password: passwordForm.current_password,
      new_password: passwordForm.new_password
    })
    passwordForm.current_password = ''
    passwordForm.new_password = ''
    passwordForm.confirm_password = ''
    passwordSuccess.value = true
  } catch (err) {
    const apiErr = err as ApiError
    passwordError.value = apiErr.error || t('account.updatePasswordFailed')
    passwordForm.current_password = ''
    passwordForm.new_password = ''
    passwordForm.confirm_password = ''
  } finally {
    passwordLoading.value = false
  }
}

onMounted(loadProfile)
useHead({ title: () => t('account.title') + ' · ' + t('common.appName') })
</script>

<template>
  <div class="min-h-screen bg-default">
    <AppHeader />
    <main class="mx-auto max-w-3xl px-4 py-8 sm:px-6">
      <h1 class="mb-6 text-2xl font-bold text-highlighted">{{ t('account.title') }}</h1>

      <div v-if="loading" class="flex justify-center py-12">
        <UIcon name="i-lucide-loader-circle" class="h-8 w-8 animate-spin text-muted" />
      </div>
      <ErrorDisplay v-else-if="loadError" :message="loadError" />

      <template v-else>
        <UCard class="mb-6">
          <template #header>
            <div>
              <h2 class="text-lg font-semibold text-highlighted">{{ t('account.profile') }}</h2>
              <p class="text-sm text-muted">{{ profile?.username }}</p>
            </div>
          </template>
          <div class="grid gap-3 text-sm sm:grid-cols-2">
            <div>
              <p class="text-muted">{{ t('account.username') }}</p>
              <p class="font-medium text-default">{{ profile?.username }}</p>
            </div>
            <div>
              <p class="text-muted">{{ t('account.systemRole') }}</p>
              <p class="font-medium text-default">{{ systemRoleLabel(profile?.system_role) }}</p>
            </div>
          </div>
        </UCard>

        <UCard class="mb-6">
          <template #header>
            <h2 class="text-lg font-semibold text-highlighted">{{ t('account.changeEmail') }}</h2>
          </template>
          <form class="space-y-4" @submit.prevent="updateEmail">
            <UFormField :label="t('account.email')">
              <UInput v-model="emailForm.email" type="email" autocomplete="email" class="w-full" />
            </UFormField>
            <UFormField :label="t('account.currentPassword')">
              <UInput v-model="emailForm.current_password" type="password" autocomplete="current-password" class="w-full" />
            </UFormField>
            <ErrorDisplay v-if="emailError" :message="emailError" />
            <p v-if="emailSuccess" class="text-sm text-success">{{ t('account.emailUpdated') }}</p>
            <div class="flex justify-end">
              <UButton type="submit" :loading="emailLoading">{{ t('common.save') }}</UButton>
            </div>
          </form>
        </UCard>

        <UCard>
          <template #header>
            <h2 class="text-lg font-semibold text-highlighted">{{ t('account.changePassword') }}</h2>
          </template>
          <form class="space-y-4" @submit.prevent="updatePassword">
            <UFormField :label="t('account.currentPassword')">
              <UInput v-model="passwordForm.current_password" type="password" autocomplete="current-password" class="w-full" />
            </UFormField>
            <UFormField :label="t('account.newPassword')">
              <UInput v-model="passwordForm.new_password" type="password" autocomplete="new-password" class="w-full" />
            </UFormField>
            <UFormField :label="t('account.confirmPassword')">
              <UInput v-model="passwordForm.confirm_password" type="password" autocomplete="new-password" class="w-full" />
            </UFormField>
            <ErrorDisplay v-if="passwordError" :message="passwordError" />
            <p v-if="passwordSuccess" class="text-sm text-success">{{ t('account.passwordUpdated') }}</p>
            <div class="flex justify-end">
              <UButton type="submit" :loading="passwordLoading">{{ t('common.save') }}</UButton>
            </div>
          </form>
        </UCard>
      </template>
    </main>
  </div>
</template>
