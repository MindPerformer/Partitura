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

  it('zh.json 默认语言包含中文文本', () => {
    expect(zh.common.appName).toContain('工作区')
    expect(zh.auth.signIn).toContain('登录')
    expect(zh.search.noResults).toContain('未找到')
  })

  it('en.json 包含英文文本', () => {
    expect(en.common.appName).toContain('Workspace')
    expect(en.auth.signIn).toContain('Sign')
    expect(en.search.noResults).toContain('No results')
  })
})
