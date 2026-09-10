// tests/markdown-outline.test.ts — Markdown heading 锚点与目录数据测试
//
// 引入动机：阅读页目录必须与实际渲染出的标题一一对应，并能定位到真实 DOM 节点。
// 测试直接调用 Markdown 渲染逻辑，覆盖 id 生成、重复标题、中文标题和代码围栏。

import { describe, it, expect } from 'vitest'
import { renderMarkdown, renderMarkdownWithOutline, slugify } from '~/utils/markdown'

describe('Markdown heading anchors', () => {
  it('为标题生成 id，并返回与 HTML 一致的目录', () => {
    const result = renderMarkdownWithOutline('# Introduction\n\n## Setup\n\n### Details')
    const headingTags = result.html.match(/<h[1-6][^>]*>/g) || []

    expect(result.headings).toHaveLength(3)
    expect(headingTags).toHaveLength(result.headings.length)
    for (const heading of result.headings) {
      expect(heading.id).not.toBe('')
      expect(result.html).toContain(`id="${heading.id}"`)
    }
  })

  it('重复标题使用稳定的递增 id', () => {
    const result = renderMarkdownWithOutline('# Same\n\n## Same\n\n### Same')

    expect(result.headings.map(heading => heading.id)).toEqual(['heading-same', 'heading-same-1', 'heading-same-2'])
  })

  it('中文标题生成非空锚点', () => {
    expect(slugify('项目说明')).toBe('项目说明')
    const result = renderMarkdownWithOutline('# 项目说明')

    expect(result.headings[0]?.id).toBe('heading-项目说明')
    expect(result.html).toContain('id="heading-项目说明"')
  })

  it('代码围栏中的井号注释不会进入目录', () => {
    const result = renderMarkdownWithOutline('```bash\n# 注释\necho ready\n```\n\n# 真实标题')

    expect(result.headings).toEqual([{ id: 'heading-真实标题', level: 1, text: '真实标题' }])
    expect(result.html.match(/<h[1-6][^>]*>/g)).toHaveLength(1)
  })

  it('每个目录项都能命中真实渲染出的 DOM 标题', () => {
    const result = renderMarkdownWithOutline('# Hello **world**\n\n## 中文标题')
    document.body.innerHTML = result.html

    for (const heading of result.headings) {
      const target = document.getElementById(heading.id)
      expect(target).not.toBeNull()
      expect(target?.textContent?.trim()).toBe(heading.text)
    }
  })

  it('renderMarkdown 保持原有字符串 API，同时输出标题 id', () => {
    const html = renderMarkdown('# Title')

    expect(html).toContain('<h1 id="heading-title">Title</h1>')
  })
})
