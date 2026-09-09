// tests/tree.test.ts — 文档树构建测试
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 Sidebar 显示 document tree。
// 测试从扁平文档列表构建正确的树形结构。

import { describe, it, expect } from 'vitest'
import type { DocumentListItem } from '~/types/api'

function makeDoc(path: string, title?: string): DocumentListItem {
  return {
    id: `doc-${path}`,
    path,
    title: title || path,
    type: '',
    status: 'active',
    content_hash: 'hash',
    revision_number: 1,
    is_special: false,
    updated_by: 'user-1',
    updated_at: '2025-01-01T00:00:00Z'
  }
}

describe('Document Tree', () => {
  it('扁平文档列表构建单层树', async () => {
    const { buildDocumentTree } = await import('~/utils/tree')
    const docs = [makeDoc('PROJECT.md'), makeDoc('README.md')]
    const tree = buildDocumentTree(docs)

    expect(tree).toHaveLength(2)
    expect(tree[0]!.isDirectory).toBe(false)
    expect(tree[0]!.label).toBe('PROJECT.md')
    expect(tree[1]!.label).toBe('README.md')
  })

  it('嵌套路径构建多层树', async () => {
    const { buildDocumentTree } = await import('~/utils/tree')
    const docs = [
      makeDoc('architecture/overview.md'),
      makeDoc('architecture/components.md'),
      makeDoc('PROJECT.md')
    ]
    const tree = buildDocumentTree(docs)

    expect(tree).toHaveLength(2)
    // 目录在前
    expect(tree[0]!.isDirectory).toBe(true)
    expect(tree[0]!.label).toBe('architecture')
    expect(tree[0]!.children).toHaveLength(2)
    // 文件在后
    expect(tree[1]!.isDirectory).toBe(false)
    expect(tree[1]!.label).toBe('PROJECT.md')
  })

  it('深层嵌套路径正确构建', async () => {
    const { buildDocumentTree } = await import('~/utils/tree')
    const docs = [
      makeDoc('a/b/c/deep.md'),
      makeDoc('a/b/other.md'),
      makeDoc('a/top.md')
    ]
    const tree = buildDocumentTree(docs)

    expect(tree).toHaveLength(1)
    expect(tree[0]!.label).toBe('a')
    expect(tree[0]!.children).toHaveLength(2)
    // top.md 是文件，b 是目录，目录在前
    const dirB = tree[0]!.children[0]!
    expect(dirB.isDirectory).toBe(true)
    expect(dirB.label).toBe('b')
    expect(dirB.children).toHaveLength(2)
    // c 目录在前
    const dirC = dirB.children[0]!
    expect(dirC.isDirectory).toBe(true)
    expect(dirC.label).toBe('c')
    expect(dirC.children).toHaveLength(1)
    expect(dirC.children[0]!.label).toBe('deep.md')
  })

  it('空列表返回空树', async () => {
    const { buildDocumentTree } = await import('~/utils/tree')
    const tree = buildDocumentTree([])
    expect(tree).toHaveLength(0)
  })

  it('目录排序在前，文件在后，各自字母序', async () => {
    const { buildDocumentTree } = await import('~/utils/tree')
    const docs = [
      makeDoc('z-file.md'),
      makeDoc('a-dir/content.md'),
      makeDoc('m-file.md'),
      makeDoc('b-dir/content.md')
    ]
    const tree = buildDocumentTree(docs)

    // 目录在前：a-dir, b-dir
    expect(tree[0]!.isDirectory).toBe(true)
    expect(tree[0]!.label).toBe('a-dir')
    expect(tree[1]!.isDirectory).toBe(true)
    expect(tree[1]!.label).toBe('b-dir')
    // 文件在后：m-file.md, z-file.md
    expect(tree[2]!.isDirectory).toBe(false)
    expect(tree[2]!.label).toBe('m-file.md')
    expect(tree[3]!.isDirectory).toBe(false)
    expect(tree[3]!.label).toBe('z-file.md')
  })

  it('findNodeInTree 查找文件节点', async () => {
    const { buildDocumentTree, findNodeInTree } = await import('~/utils/tree')
    const docs = [
      makeDoc('architecture/overview.md'),
      makeDoc('PROJECT.md')
    ]
    const tree = buildDocumentTree(docs)

    const node = findNodeInTree(tree, 'architecture/overview.md')
    expect(node).not.toBeNull()
    expect(node!.isDirectory).toBe(false)
    expect(node!.path).toBe('architecture/overview.md')
  })

  it('findNodeInTree 查找不存在的路径返回 null', async () => {
    const { buildDocumentTree, findNodeInTree } = await import('~/utils/tree')
    const docs = [makeDoc('PROJECT.md')]
    const tree = buildDocumentTree(docs)

    const node = findNodeInTree(tree, 'nonexistent.md')
    expect(node).toBeNull()
  })

  it('文件节点关联 DocumentListItem', async () => {
    const { buildDocumentTree } = await import('~/utils/tree')
    const doc = makeDoc('test.md', 'Test Title')
    const tree = buildDocumentTree([doc])

    expect(tree[0]!.document).toBeDefined()
    expect(tree[0]!.document!.title).toBe('Test Title')
  })
})
