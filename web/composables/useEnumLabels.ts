// composables/useEnumLabels.ts — 将后端枚举值转换为当前语言的用户可见文案。
//
// 引入动机：API 保持稳定的英文枚举值，Web 展示层不能直接把
// system_admin、owner、active 等内部值渲染给用户。

export function useEnumLabels() {
  const { t } = useI18n()

  function translate(key: string, value: string): string {
    const translated = t(key)
    if (translated === key) {
      if (import.meta.dev) {
        console.warn(`[enum-labels] 缺少翻译键 ${key}，回退显示枚举值 ${value}`)
      }
      return value
    }
    return translated
  }

  function unknownLabel(kind: string, value: string): string {
    if (import.meta.dev) {
      console.warn(`[enum-labels] 未知${kind}枚举值 ${value}`)
    }
    return value
  }

  function systemRoleLabel(role: string | null | undefined): string {
    if (!role) return '-'
    const key = role === 'system_admin' ? 'admin.roleSystemAdmin' : role === 'user' ? 'admin.roleUser' : ''
    return key ? translate(key, role) : unknownLabel('系统角色', role)
  }

  function workspaceRoleLabel(role: string | null | undefined): string {
    if (!role) return '-'
    const keys: Record<string, string> = {
      viewer: 'workspace.roleViewer',
      editor: 'workspace.roleEditor',
      admin: 'workspace.roleAdmin',
      owner: 'workspace.roleOwner'
    }
    return keys[role] ? translate(keys[role], role) : unknownLabel('工作区角色', role)
  }

  function workspaceStatusLabel(status: string | null | undefined): string {
    if (!status) return '-'
    const keys: Record<string, string> = {
      active: 'workspace.statusActive',
      archived: 'workspace.statusArchived'
    }
    return keys[status] ? translate(keys[status], status) : unknownLabel('工作区状态', status)
  }

  function documentStatusLabel(status: string | null | undefined): string {
    if (!status) return '-'
    const keys: Record<string, string> = {
      active: 'document.statusActive',
      archived: 'document.statusArchived'
    }
    return keys[status] ? translate(keys[status], status) : unknownLabel('文档状态', status)
  }

  function profileStatusLabel(status: string | null | undefined): string {
    if (!status) return '-'
    const keys: Record<string, string> = {
      active: 'admin.statusActive',
      draft: 'admin.statusDraft',
      inactive: 'admin.statusInactive',
      archived: 'admin.statusArchived'
    }
    return keys[status] ? translate(keys[status], status) : unknownLabel('搜索配置状态', status)
  }

  function jobStatusLabel(status: string | null | undefined): string {
    if (!status) return '-'
    const keys: Record<string, string> = {
      pending: 'admin.statusPending',
      running: 'admin.statusRunning',
      completed: 'admin.statusCompleted',
      failed: 'admin.statusFailed',
      dead: 'admin.statusDead'
    }
    return keys[status] ? translate(keys[status], status) : unknownLabel('任务状态', status)
  }

  function documentTypeLabel(type: string | null | undefined): string {
    if (!type) return '-'
    const keys: Record<string, string> = {
      architecture: 'document.typeArchitecture',
      codebase: 'document.typeCodebase',
      development: 'document.typeDevelopment',
      decision: 'document.typeDecision',
      issue: 'document.typeIssue',
      roadmap: 'document.typeRoadmap',
      research: 'document.typeResearch',
      reference: 'document.typeReference',
      operation: 'document.typeOperation',
      standard: 'document.typeStandard',
      guide: 'document.typeGuide',
      other: 'document.typeOther'
    }
    return keys[type] ? translate(keys[type], type) : unknownLabel('文档类型', type)
  }

  function sourceTypeLabel(type: string | null | undefined): string {
    if (!type) return '-'
    const keys: Record<string, string> = {
      web: 'document.sourceTypeWeb',
      code: 'document.sourceTypeCode',
      file: 'document.sourceTypeFile',
      issue: 'document.sourceTypeIssue',
      commit: 'document.sourceTypeCommit',
      conversation: 'document.sourceTypeConversation',
      manual: 'document.sourceTypeManual',
      other: 'document.sourceTypeOther'
    }
    return keys[type] ? translate(keys[type], type) : unknownLabel('来源类型', type)
  }

  function jobTypeLabel(type: string | null | undefined): string {
    if (!type) return '-'
    const keys: Record<string, string> = {
      index_document: 'admin.jobTypeIndexDocument',
      rebuild_index: 'admin.jobTypeRebuildIndex',
      repair_index: 'admin.jobTypeRepairIndex',
      evaluate_profile: 'admin.jobTypeEvaluateProfile',
      optimize_profile: 'admin.jobTypeOptimizeProfile',
      cleanup_old_indexes: 'admin.jobTypeCleanupOldIndexes',
      cleanup_revisions: 'admin.jobTypeCleanupRevisions'
    }
    return keys[type] ? translate(keys[type], type) : unknownLabel('任务类型', type)
  }

  function datasetStatusLabel(status: string | null | undefined): string {
    if (!status) return '-'
    const keys: Record<string, string> = {
      active: 'admin.statusActive',
      archived: 'admin.statusArchived'
    }
    return keys[status] ? translate(keys[status], status) : unknownLabel('数据集状态', status)
  }

  function settingTypeLabel(type: string | null | undefined): string {
    if (!type) return '-'
    // 后端 SystemSetting.type 词汇为 string/int/bool/float/duration；
    // json 保留以兼容后端可能扩展返回的类型。
    const keys: Record<string, string> = {
      string: 'admin.settingTypeString',
      bool: 'admin.settingTypeBoolean',
      int: 'admin.settingTypeInteger',
      float: 'admin.settingTypeNumber',
      duration: 'admin.settingTypeDuration',
      json: 'admin.settingTypeJson'
    }
    return keys[type] ? translate(keys[type], type) : unknownLabel('配置类型', type)
  }

  return {
    systemRoleLabel,
    workspaceRoleLabel,
    workspaceStatusLabel,
    documentStatusLabel,
    profileStatusLabel,
    jobStatusLabel,
    documentTypeLabel,
    sourceTypeLabel,
    jobTypeLabel,
    datasetStatusLabel,
    settingTypeLabel
  }
}
