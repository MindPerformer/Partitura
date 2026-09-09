// utils/diff.ts — 文本差异计算
//
// 引入动机：design/04-WEB-API.md §页面要求 Diff Viewer 页面。
// 使用 diff 库计算两个文本版本之间的行级差异。
// 避免伪静态差异——实际对比两个 revision 的 content_markdown。

import { diffLines, diffWords } from 'diff'

export interface DiffLine {
  type: 'added' | 'removed' | 'unchanged'
  content: string
  oldLineNumber?: number
  newLineNumber?: number
}

/**
 * 计算两个文本之间的行级差异。
 *
 * 引入动机：Revision History 页面需要对比当前版本与历史版本，
 * 或任意两个历史版本之间的差异。
 *
 * @param oldText — 旧版本文本
 * @param newText — 新版本文本
 * @returns DiffLine 数组，每行标注 added/removed/unchanged
 */
export function computeLineDiff(oldText: string, newText: string): DiffLine[] {
  const changes = diffLines(oldText, newText)
  const result: DiffLine[] = []
  let oldLineNum = 1
  let newLineNum = 1

  for (const change of changes) {
    const lines = change.value.split('\n')
    // 移除最后一个空元素（如果文本以 \n 结尾）
    if (lines.length > 0 && lines[lines.length - 1] === '') {
      lines.pop()
    }

    for (const line of lines) {
      if (change.added) {
        result.push({
          type: 'added',
          content: line,
          newLineNumber: newLineNum
        })
        newLineNum++
      } else if (change.removed) {
        result.push({
          type: 'removed',
          content: line,
          oldLineNumber: oldLineNum
        })
        oldLineNum++
      } else {
        result.push({
          type: 'unchanged',
          content: line,
          oldLineNumber: oldLineNum,
          newLineNumber: newLineNum
        })
        oldLineNum++
        newLineNum++
      }
    }
  }

  return result
}

export interface DiffStats {
  additions: number
  deletions: number
  unchanged: number
}

/**
 * 计算差异统计信息。
 */
export function computeDiffStats(diffLines: DiffLine[]): DiffStats {
  let additions = 0
  let deletions = 0
  let unchanged = 0

  for (const line of diffLines) {
    switch (line.type) {
      case 'added':
        additions++
        break
      case 'removed':
        deletions++
        break
      case 'unchanged':
        unchanged++
        break
    }
  }

  return { additions, deletions, unchanged }
}

/**
 * 计算两个文本之间的词级差异（用于 inline diff 显示）。
 *
 * @param oldText — 旧版本文本
 * @param newText — 新版本文本
 * @returns HTML 字符串，added 部分用 <ins> 包裹，removed 部分用 <del> 包裹
 */
export function computeWordDiff(oldText: string, newText: string): string {
  const changes = diffWords(oldText, newText)
  return changes
    .map(change => {
      if (change.added) {
        return `<ins class="diff-word-added">${escapeHtml(change.value)}</ins>`
      }
      if (change.removed) {
        return `<del class="diff-word-removed">${escapeHtml(change.value)}</del>`
      }
      return escapeHtml(change.value)
    })
    .join('')
}

function escapeHtml(text: string): string {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;')
}
