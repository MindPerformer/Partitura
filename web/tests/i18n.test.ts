// tests/i18n.test.ts — i18n 词典完整性测试
//
// 引入动机：Phase 6 要求完整中英 i18n，需验证：
// 1. zh.json 和 en.json 结构一致（相同 key 树）
// 2. 关键用户可见 key 在两种语言中都存在
// 3. 不存在空翻译值

import { describe, it, expect } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const zh = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'zh.json'), 'utf-8'))
const en = JSON.parse(readFileSync(resolve(__dirname, '..', 'locales', 'en.json'), 'utf-8'))

function collectKeys(obj: Record<string, unknown>, prefix = ''): string[] {
  const keys: string[] = []
  for (const [key, value] of Object.entries(obj)) {
    const fullKey = prefix ? `${prefix}.${key}` : key
    if (typeof value === 'object' && value !== null && !Array.isArray(value)) {
      keys.push(...collectKeys(value as Record<string, unknown>, fullKey))
    } else {
      keys.push(fullKey)
    }
  }
  return keys
}

describe('i18n 词典完整性', () => {
  it('zh.json 和 en.json 拥有相同的 key 结构', () => {
    const zhKeys = collectKeys(zh).sort()
    const enKeys = collectKeys(en).sort()
    expect(zhKeys).toEqual(enKeys)
  })

  it('不存在空翻译值', () => {
    const zhKeys = collectKeys(zh)
    for (const key of zhKeys) {
      const parts = key.split('.')
      let val: unknown = zh
      for (const p of parts) {
        val = (val as Record<string, unknown>)[p]
      }
      expect(val, `zh.json key "${key}" should not be empty`).toBeTruthy()
    }

    const enKeys = collectKeys(en)
    for (const key of enKeys) {
      const parts = key.split('.')
      let val: unknown = en
      for (const p of parts) {
        val = (val as Record<string, unknown>)[p]
      }
      expect(val, `en.json key "${key}" should not be empty`).toBeTruthy()
    }
  })

  it('角色和状态枚举具有中英文用户可见文案', () => {
    const translatedValues = [
      ['admin.roleSystemAdmin', zh.admin.roleSystemAdmin, en.admin.roleSystemAdmin, 'system_admin'],
      ['admin.roleUser', zh.admin.roleUser, en.admin.roleUser, 'user'],
      ['workspace.roleOwner', zh.workspace.roleOwner, en.workspace.roleOwner, 'owner'],
      ['workspace.roleAdmin', zh.workspace.roleAdmin, en.workspace.roleAdmin, 'admin'],
      ['workspace.roleEditor', zh.workspace.roleEditor, en.workspace.roleEditor, 'editor'],
      ['workspace.roleViewer', zh.workspace.roleViewer, en.workspace.roleViewer, 'viewer'],
      ['workspace.statusActive', zh.workspace.statusActive, en.workspace.statusActive, 'active'],
      ['workspace.statusArchived', zh.workspace.statusArchived, en.workspace.statusArchived, 'archived'],
      ['document.statusActive', zh.document.statusActive, en.document.statusActive, 'active'],
      ['document.statusArchived', zh.document.statusArchived, en.document.statusArchived, 'archived'],
      ['admin.statusDraft', zh.admin.statusDraft, en.admin.statusDraft, 'draft'],
      ['admin.statusPending', zh.admin.statusPending, en.admin.statusPending, 'pending'],
      ['admin.statusRunning', zh.admin.statusRunning, en.admin.statusRunning, 'running'],
      ['admin.statusCompleted', zh.admin.statusCompleted, en.admin.statusCompleted, 'completed'],
      ['admin.statusFailed', zh.admin.statusFailed, en.admin.statusFailed, 'failed'],
      ['admin.jobTypeIndexDocument', zh.admin.jobTypeIndexDocument, en.admin.jobTypeIndexDocument, 'index_document'],
      ['admin.settingTypeString', zh.admin.settingTypeString, en.admin.settingTypeString, 'string']
    ] as const

    for (const [key, zhValue, enValue, enumValue] of translatedValues) {
      expect(zhValue, `zh.json missing translation for ${key}`).toBeTruthy()
      expect(enValue, `en.json missing translation for ${key}`).toBeTruthy()
      expect(zhValue).not.toBe(enumValue)
      expect(enValue).not.toBe(enumValue)
    }
  })

  it('关键用户可见 key 在两种语言中都存在', () => {
    const criticalKeys = [
      'common.appName',
      'common.save',
      'common.cancel',
      'common.search',
      'common.back',
      'common.loading',
      'auth.signIn',
      'auth.signOut',
      'auth.username',
      'auth.password',
      'auth.invalidCredentials',
      'workspace.workspaces',
      'workspace.newWorkspace',
      'workspace.members',
      'workspace.settings',
      'document.documents',
      'document.newDocument',
      'document.editDocument',
      'document.title',
      'document.content',
      'search.title',
      'search.searchInWorkspace',
      'search.noResults',
      'admin.adminUsers',
      'admin.adminWorkspaces',
      'admin.searchProfiles',
      'admin.indexJobs',
      'admin.auditLog',
      'device.title',
      'health.title',
      'errors.versionConflict',
      'errors.versionConflictDesc',
      'pagination.range',
      'pagination.page'
    ]

    const zhKeys = new Set(collectKeys(zh))
    const enKeys = new Set(collectKeys(en))

    for (const key of criticalKeys) {
      expect(zhKeys.has(key), `zh.json missing critical key: ${key}`).toBe(true)
      expect(enKeys.has(key), `en.json missing critical key: ${key}`).toBe(true)
    }
  })

  it('品牌名称统一为 Partitura', () => {
    expect(zh.common.appName).toBe('Partitura')
    expect(en.common.appName).toBe('Partitura')
  })

  it('zh.json 默认语言包含中文文本', () => {
    expect(zh.auth.signIn).toContain('登录')
    expect(zh.search.noResults).toContain('未找到')
  })

  it('en.json 包含英文文本', () => {
    expect(en.auth.signIn).toContain('Sign')
    expect(en.search.noResults).toContain('No results')
  })
})
