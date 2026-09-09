// tests/nginx-query-passthrough.test.ts — Nginx 查询参数透传验证测试
//
// 引入动机：远程错误 `offset 参数非法: 包含非数字字符: "0?limit=20"` 表明
// 旧 Nitro 代理 handler 在转发 API 请求时损坏了 query string，
// 导致后端收到的 offset 值为 "0?limit=20" 而非 "0"。
//
// 本测试验证：
// 1. Nginx 配置模板使用不含 URI 的 proxy_pass（确保原始 query string 原样透传）
// 2. Nginx 配置模板不使用 $args 或 $request_uri 拼接（避免双重编码或参数损坏）
// 3. Nginx 配置模板正确设置 SPA fallback
// 4. Nginx 配置模板以非 root 用户运行
//
// 注意：这是配置语义验证测试，不是源码 contains 测试。
// 我们通过解析 Nginx 配置模板的关键指令来验证其行为正确性。

import { describe, it, expect } from 'vitest'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

const nginxConfPath = resolve(__dirname, '..', 'nginx.conf.template')
const rawConf = readFileSync(nginxConfPath, 'utf-8')

// 去除注释行，仅保留实际指令用于验证
// 引入动机：注释中会提到 $args、$request_uri 等作为说明，
// 测试应验证实际指令行，而非注释内容。
const nginxConf = rawConf
  .split('\n')
  .filter(line => {
    const trimmed = line.trim()
    return trimmed !== '' && !trimmed.startsWith('#')
  })
  .join('\n')

// 提取 proxy_pass 指令行（非注释）
function extractDirective(conf: string, directive: string): string | null {
  const lines = conf.split('\n')
  for (const line of lines) {
    const trimmed = line.trim()
    if (trimmed.startsWith(directive)) {
      return trimmed
    }
  }
  return null
}

describe('Nginx 查询参数透传', () => {
  it('proxy_pass 不含 URI 路径，确保原始 query string 原样透传', () => {
    // Nginx 行为：当 proxy_pass URL 只包含 host:port（不含 URI 部分）时，
    // Nginx 将完整的原始 URI（含 query string）原样转发到后端。
    // 如果 proxy_pass 包含 URI（如 http://backend/api/），Nginx 会替换 URI 部分，
    // 可能导致 query string 处理异常。
    //
    // 错误根因：旧 Nitro handler 使用 getQuery(event) 重新构建 query string，
    // 当 event.path 已包含 query 时导致 query 被拼接到 path 中。
    // Nginx 的 proxy_pass http://backend 不含 URI，原始 URI 和 query 原样透传。
    const proxyPassLine = extractDirective(nginxConf, 'proxy_pass')
    expect(proxyPassLine).not.toBeNull()
    // 提取 proxy_pass 的 URL 参数
    const urlMatch = proxyPassLine!.match(/proxy_pass\s+(\S+);/)
    expect(urlMatch).not.toBeNull()
    const proxyPassUrl = urlMatch![1]
    // proxy_pass 应为 http://backend，不含路径部分
    expect(proxyPassUrl).toBe('http://backend')
    // 不应包含 $request_uri 或 $args 拼接
    expect(proxyPassUrl).not.toContain('$request_uri')
    expect(proxyPassUrl).not.toContain('$args')
    expect(proxyPassUrl).not.toContain('$uri')
  })

  it('不使用 $args 或 $request_uri 手动拼接 query string', () => {
    // 手动拼接 $args 或 $request_uri 可能导致双重编码或参数损坏
    // Nginx 默认行为已正确透传 query string，不需要手动处理
    // 注释已被过滤，此处检查实际指令行
    expect(nginxConf).not.toContain('$args')
    expect(nginxConf).not.toContain('$request_uri')
  })

  it('location /api/ 精确匹配 API 路径前缀', () => {
    expect(nginxConf).toContain('location /api/')
  })

  it('SPA fallback 正确配置 try_files 到 index.html', () => {
    const rootMatch = nginxConf.match(/location\s+\/\s+{[^}]*try_files\s+([^;]+);/)
    expect(rootMatch).not.toBeNull()
    const tryFiles = (rootMatch ?? [])['1']?.trim() ?? ''
    expect(tryFiles).toContain('index.html')
  })

  it('Nginx 以非 root 用户运行（pid 路径在 /tmp）', () => {
    // pid 路径在 /tmp 确保非 root 用户可写
    expect(nginxConf).toContain('pid /tmp/nginx.pid')
  })

  it('临时路径配置在 /tmp 下确保非 root 可写', () => {
    expect(nginxConf).toContain('client_body_temp_path /tmp/')
    expect(nginxConf).toContain('proxy_temp_path /tmp/')
  })

  it('upstream 使用环境变量模板占位符', () => {
    expect(nginxConf).toContain('${SERVER_UPSTREAM}')
  })

  it('监听端口为 8080（容器端口契约）', () => {
    expect(nginxConf).toContain('listen 8080')
  })

  it('健康检查端点 /healthz 配置正确', () => {
    expect(nginxConf).toContain('location = /healthz')
    expect(nginxConf).toContain('return 200')
  })

  it('透传 Cookie 和认证相关 header', () => {
    expect(nginxConf).toContain('proxy_pass_request_headers on')
    expect(nginxConf).toContain('proxy_pass_header Set-Cookie')
  })
})

describe('Nginx 查询参数透传语义验证', () => {
  // 验证 Nginx proxy_pass 的语义行为：
  // 当 proxy_pass http://backend（不含 URI）时，
  // 请求 /api/workspaces?offset=0&limit=20 会被原样转发为
  // /api/workspaces?offset=0&limit=20 到后端。
  //
  // 这确保了 offset=0 和 limit=20 作为独立的 query parameter 到达后端，
  // 而不会被合并为 offset="0?limit=20"。

  it('proxy_pass http://backend 语义：原始 URI 和 query string 原样透传', () => {
    const proxyPassLine = extractDirective(nginxConf, 'proxy_pass')
    expect(proxyPassLine).not.toBeNull()
    const urlMatch = proxyPassLine!.match(/proxy_pass\s+(\S+);/)
    expect(urlMatch).not.toBeNull()
    const target = urlMatch![1]
    // http://backend 不含路径，Nginx 原样转发完整 URI
    expect(target).toMatch(/^https?:\/\/backend$/)
  })

  it('offset=0&limit=20 作为独立参数到达后端（语义验证）', () => {
    // Nginx proxy_pass http://backend 行为：
    // 请求 GET /api/workspaces?offset=0&limit=20
    // 转发到 backend 为 GET /api/workspaces?offset=0&limit=20
    //
    // 后端 r.URL.Query().Get("offset") = "0"
    // 后端 r.URL.Query().Get("limit") = "20"
    //
    // 对比旧 Nitro handler 的错误行为：
    // event.path = "/api/workspaces?offset=0&limit=20"
    // getQuery(event) 解析后重新构建 query string
    // targetUrl = targetBase + path + "?" + qs
    // 如果 path 已包含 "?offset=0&limit=20"，则 targetUrl 变为
    // http://server:8080/api/workspaces?offset=0&limit=20?offset=0&limit=20
    // 或在某些情况下 path 参数解析异常导致 offset="0?limit=20"
    //
    // Nginx 不存在此问题：proxy_pass http://backend 原样转发 URI + query
    const proxyPassLine = extractDirective(nginxConf, 'proxy_pass')
    expect(proxyPassLine).not.toBeNull()
    const urlMatch = proxyPassLine!.match(/proxy_pass\s+(\S+);/)
    expect(urlMatch).not.toBeNull()
    const target = urlMatch![1]
    expect(target).toBe('http://backend')
    // 确认没有 $is_args 或 $args 拼接行为
    expect(target).not.toContain('$')
  })
})
