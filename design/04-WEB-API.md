# Web, API and RBAC

## Frontend

Nuxt + Nuxt UI + TypeScript。

文档体验参考 VitePress / GitBook：

Topbar:
- workspace
- search
- user

Sidebar:
- document tree

Main:
- Markdown page/editor

Right:
- Table of Contents
- metadata
- sources
- revision info

## 页面

- Login
- Workspace List
- Workspace Home
- Document Reader
- Document Editor
- Search
- Revision History
- Diff Viewer
- Workspace Members
- Workspace Settings
- API / Device Sessions
- Admin Users
- Admin Workspaces
- Search Profiles
- Search Evaluation
- Index Jobs
- Audit Log
- System Health

## RBAC

System:
- system_admin
- user

Workspace:
- owner
- admin
- editor
- viewer

viewer:
- read
- search
- history

editor:
- viewer +
- create
- update
- move

admin:
- editor +
- archive
- member management
- settings

owner:
- admin +
- transfer
- purge/delete workspace

Workspace 创建权限独立：
- workspace:create

默认普通用户不一定拥有。

## Search

Web search 只搜索当前打开的 Workspace。

没有跨 Workspace 搜索。

用户要搜另一个项目：
- 切换 Workspace
- 再搜索

## API

服务器 REST API 至少覆盖：

Auth / Device:
- login/session
- device authorization
- refresh/revoke

Workspace:
- list
- get
- create
- update
- archive
- members

Document:
- list
- outline
- read
- read section
- read lines
- create
- patch/replace
- move
- archive
- history
- revision
- sources

Search:
- search current workspace
- feedback

Admin:
- users
- workspace permissions
- search profiles
- evaluation
- jobs
- retention config
- audit
- health

## Concurrency

任何 update：
- expected_revision
- expected_hash

不匹配：
- HTTP 409

禁止 silent overwrite。

## Security

- Argon2id password hashing
- secure cookies
- CSRF protection
- strict path validation
- safe Markdown rendering
- XSS prevention
- token hashes only
- ACL 后端强制

Elasticsearch query 只能由服务器生成。

客户端不能自定义 workspace filter 绕过权限。
