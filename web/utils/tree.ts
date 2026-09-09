// utils/tree.ts — 文档树构建
//
// 引入动机：design/04-WEB-API.md §Frontend 要求 Sidebar 显示 document tree。
// 从 document list 构建树形结构，按 path 的目录层级组织。

import type { DocumentListItem } from '~/types/api'

export interface TreeNode {
  /** 目录名或文件 path */
  path: string
  /** 显示名 */
  label: string
  /** 是否为目录 */
  isDirectory: boolean
  /** 子节点（仅目录有） */
  children: TreeNode[]
  /** 关联的文档（仅文件节点有） */
  document?: DocumentListItem
  /** 层级深度 */
  level: number
}

/**
 * 从文档列表构建树形结构。
 *
 * 引入动机：VitePress/GitBook 风格 sidebar 需要按目录层级展示文档。
 * path 格式如 "architecture/overview.md"、"PROJECT.md"。
 *
 * 行为：
 * - 按 path 的 "/" 分割为目录层级
 * - 目录节点聚合子文档
 * - 文件节点关联 DocumentListItem
 * - 排序：目录在前，文件在后，按 label 字母序
 *
 * @param documents — 文档列表
 * @returns 树形根节点列表
 */
export function buildDocumentTree(documents: DocumentListItem[]): TreeNode[] {
  const root: TreeNode = {
    path: '',
    label: '',
    isDirectory: true,
    children: [],
    level: 0
  }

  for (const doc of documents) {
    const parts = doc.path.split('/')
    let currentNode = root

    for (let i = 0; i < parts.length; i++) {
      const part = parts[i]!
      const isLast = i === parts.length - 1
      const currentPath = parts.slice(0, i + 1).join('/')

      if (isLast) {
        // 文件节点
        const existing = currentNode.children.find(n => n.path === currentPath && !n.isDirectory)
        if (!existing) {
          currentNode.children.push({
            path: currentPath,
            label: part,
            isDirectory: false,
            children: [],
            document: doc,
            level: i + 1
          })
        }
      } else {
        // 目录节点
        let dirNode: TreeNode | undefined = currentNode.children.find(n => n.path === currentPath && n.isDirectory)
        if (!dirNode) {
          dirNode = {
            path: currentPath,
            label: part,
            isDirectory: true,
            children: [],
            level: i + 1
          }
          currentNode.children.push(dirNode)
        }
        currentNode = dirNode
      }
    }
  }

  // 递归排序：目录在前，文件在后
  sortTree(root)

  return root.children
}

/**
 * 递归排序树节点：目录在前，文件在后，各自按 label 字母序。
 */
function sortTree(node: TreeNode): void {
  node.children.sort((a, b) => {
    if (a.isDirectory !== b.isDirectory) {
      return a.isDirectory ? -1 : 1
    }
    return a.label.localeCompare(b.label)
  })

  for (const child of node.children) {
    sortTree(child)
  }
}

/**
 * 在树中查找指定 path 的节点。
 */
export function findNodeInTree(root: TreeNode[], path: string): TreeNode | null {
  for (const node of root) {
    if (node.path === path && !node.isDirectory) {
      return node
    }
    if (node.isDirectory && path.startsWith(node.path + '/')) {
      const found = findNodeInTree(node.children, path)
      if (found) return found
    }
  }
  return null
}
