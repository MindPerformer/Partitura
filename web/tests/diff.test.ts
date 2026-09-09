// tests/diff.test.ts — 行级 diff 测试
//
// 引入动机：design/03-DOCUMENTS.md 要求 revision diff 支持行级比较。
// 测试新增、删除、修改行的正确识别。

import { describe, it, expect } from 'vitest'

describe('Line-level Diff', () => {
  it('完全相同的文本无差异', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('line1\nline2\nline3', 'line1\nline2\nline3')
    expect(result).toHaveLength(3)
    result.forEach(line => {
      expect(line.type).toBe('unchanged')
    })
  })

  it('新增行被标记为 added', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('line1\nline2\n', 'line1\nline2\nline3\n')
    expect(result.some(l => l.type === 'added' && l.content === 'line3')).toBe(true)
  })

  it('删除行被标记为 removed', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('line1\nline2\nline3\n', 'line1\nline2\n')
    expect(result.some(l => l.type === 'removed' && l.content === 'line3')).toBe(true)
  })

  it('修改行被标记为 removed + added', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('line1\nold\nline3\n', 'line1\nnew\nline3\n')
    expect(result.some(l => l.type === 'removed' && l.content === 'old')).toBe(true)
    expect(result.some(l => l.type === 'added' && l.content === 'new')).toBe(true)
  })

  it('空文本对比', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('', 'new content\n')
    expect(result.some(l => l.type === 'added')).toBe(true)
  })

  it('多行连续新增', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('a\n', 'a\nb\nc\nd\n')
    const addedLines = result.filter(l => l.type === 'added')
    expect(addedLines.map(l => l.content)).toContain('b')
    expect(addedLines.map(l => l.content)).toContain('c')
    expect(addedLines.map(l => l.content)).toContain('d')
  })

  it('多行连续删除', async () => {
    const { computeLineDiff } = await import('~/utils/diff')
    const result = computeLineDiff('a\nb\nc\nd\n', 'a\n')
    const removedLines = result.filter(l => l.type === 'removed')
    expect(removedLines.map(l => l.content)).toContain('b')
    expect(removedLines.map(l => l.content)).toContain('c')
    expect(removedLines.map(l => l.content)).toContain('d')
  })

  it('computeDiffStats 正确统计', async () => {
    const { computeLineDiff, computeDiffStats } = await import('~/utils/diff')
    const lines = computeLineDiff('a\nb\nc\n', 'a\nx\nc\ny\n')
    const stats = computeDiffStats(lines)
    expect(stats.additions).toBeGreaterThanOrEqual(2) // x and y (may include empty)
    expect(stats.deletions).toBeGreaterThanOrEqual(1) // b
    expect(stats.unchanged).toBeGreaterThanOrEqual(2) // a and c
  })

  it('computeWordDiff 生成 ins/del 标签', async () => {
    const { computeWordDiff } = await import('~/utils/diff')
    const html = computeWordDiff('hello world', 'hello earth')
    expect(html).toContain('<ins')
    expect(html).toContain('earth')
    expect(html).toContain('<del')
    expect(html).toContain('world')
  })
})
