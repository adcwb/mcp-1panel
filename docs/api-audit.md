# API 定义与 MCP 实现审查

本次审查发现并修复了请求定义及传输层问题。结论是：**770 个接口已注册，不等于 770 个接口已可靠验收**。
独立模式的本地转发与参数校验已有回归测试；托管模式、终端身份及真实面板业务仍有下述限制。

## 核对依据与范围

- 契约：仓库根目录 `doc.json`，Swagger 2.0、`/api/v2`，770 个操作、728 个定义、58 个分类。
- 参考实现：[1Panel 固定提交 daae8c43](https://github.com/1Panel-dev/1Panel/tree/daae8c43f88ac4cf736d52381db156de6b98f61b)。检查了 Core/Agent API、DTO、路由、终端相关的 233 个 Go 文件。
- 按 method/path 匹配到 763 个带路由注解的处理函数，并检查直接读取的参数、绑定 DTO 和特殊返回方式。此匹配不是运行验证，也不覆盖所有服务层、间接 helper 或中间件行为。
- `GET /core/auth/welcome`、`POST /ai/ollama/model/sync` 及 `/ai/tensorrt/{create,delete,operate,search,update}` 的 5 个 POST 接口未匹配到同路径注解，保留用户契约，未推断其实现。
- 文档 `info.version=2.0` 不能确定面板发行版本。源码与文档不是已证明完全一致的版本组合。

## 已修复的请求定义

以下 13 处通过实际处理函数及 DTO 确认。修正保存在 [`scripts/api-overrides.json`](../scripts/api-overrides.json)，每条带固定版本来源；原始 `doc.json` 保持不变。

| 接口 | 问题与修正 |
| --- | --- |
| `POST /backups/refresh/token` | `BackupOperate` 改为实际使用的 `OperateByID` |
| `POST /containers/template/del` | 容器 `names` 删除结构改为模板 `ids` 删除结构 |
| `POST /databases/pg/privileges` | 使用 `name/database/username/superUser` 权限请求 |
| `POST /containers/ipv6option/update` | 错误的日志配置结构改为 IPv6 配置 |
| `POST /files/convert` | 单文件结构改为 `files/outputPath/deleteSource/taskID` 请求 |
| `POST /containers/repo` | 删除仓库结构改为创建仓库结构 |
| `POST /runtimes/node/modules/operate` | 补齐 `operate/module/pkgManager` |
| `POST /websites/redirect` | `id` 改为 `websiteId` |
| `POST /websites/ca/renew` | 申请证书结构改为续期所需的 `SSLID` |
| `POST /core/auth/mfa` | 绑定凭据结构改为生成 MFA 所需的 `title/interval` |
| `POST /core/commands/search` | 补齐排序、分组和命令类型；不能使用 Agent 的同名 DTO |
| `POST /groups/search` | 按实际 `OperateByType` 允许省略 `type` |
| `POST /core/groups/search` | 同上 |

另外补齐 `containersOnly`、容器统计 `ids`、SSL 上传 `nodes/pushNode`、容器终端 `session` 和本地终端 `title`。
此前的上传、分块下载及 SSE/WebSocket 修正继续保留。当前共 38 条端点修正，其中 12 个端点显式声明二进制返回。

## 已修复的实现问题

1. **空文件被误报为请求受理。** 原先先判断空白响应，导致零字节文件、仅含空白的文件丢失下载语义。现在下载端点始终保留字节及元数据。
2. **异常响应被误报为成功。** 普通接口原先接受无 `code` 的 JSON、空响应及任意文本/二进制。现在必须提供有效状态包装；写操作遇到不可判定结果返回“结果未知”，不自动重试。
3. **大整数被改变。** 原先把参数统一解码成 `float64`，超过 2^53 的 ID 可能被舍入。现在保留 JSON 数字，64 位整数精确转发。校验库 v0.4.3 将 `json.Number` 视为字符串，因此另行构造数值校验副本，防止错判类型；有回归用例覆盖这一行为。
4. **SSE 多行数据反复复制，事件 ID 丢失。** 改用累积缓冲并保留后续事件继承的 ID。
5. **生成器修改修正输入。** 同时存在 `parameters` 和 `extraParameters` 时原先会修改原始列表，重复生成可能添加重复参数。现已复制列表，并对重复参数、不支持的标量/头/传输及混合 body/form 提前报错。
6. **覆盖测试只证明路径数量。** 新增离线契约检查，核对全部参数的类型、必填、枚举等约束以及引用定义；增加纠错 DTO 的正反例和实际 HTTP 回归测试。

修正改变了相关工具的输入或传输契约，policy hash 随之改变。支持 endpoint-v1 的 Core 必须重新导入目录并更新授权，不能沿用旧 hash。

## 尚不能判定为可靠支持的部分

### 托管 Core/Agent 执行

本仓库仅包含 MCP 适配器。新增工具通过 `endpoint-v1` 参数封装提交给 Core；Core/Agent 的对应调度实现不在本仓库。
测试确认身份、准入票据、审计 ID 和策略 hash 的转发，以及旧契约拒绝行为，**不能证明目标 Core 已实现执行能力**。
需要在支持扩展的 Core/Agent 上联调；不应将旧 Core 拒绝这些工具视为接口业务失败。

### 终端、登录及 WebAuthn

[固定版本终端处理函数](https://github.com/1Panel-dev/1Panel/blob/daae8c43f88ac4cf736d52381db156de6b98f61b/agent/app/api/v2/terminal.go)要求面板签发的用户/认证会话身份。
API Key 和一次 WebSocket 握手不足以证明终端可用。适配器不会伪造内部身份头，也不保存共享浏览器 Cookie。
登录、Passkey/WebAuthn 的跨请求会话及浏览器交互不由当前适配器完成。WebSocket 返回的是有限时间内捕获的原始帧，不是命令业务成功凭据。

### 版本差异

另有以下 16 个操作的同名 DTO 在参考源码中包含文档未列出的字段。尚不能区分文档遗漏与版本演进，因此未直接扩展输入契约：

| 接口 | 参考源码中的额外字段 |
| --- | --- |
| `POST /ai/agents` | `accessHost` |
| `POST /ai/agents/model/update` | `metadata` |
| `POST /ai/accounts/update` | `models` |
| `POST /ai/accounts/models/create` | `taskID` |
| `POST /ai/accounts/models/delete` | `deleteFiles` |
| `POST /ai/agents/channel/feishu/update` | `groupSessionScope` |
| `POST /alert/config/update` | `revision` |
| `POST /alert/config/test` | `config/id/type` |
| `POST /apps/install` | `gpu/modelAccountBaseURL/modelAccountBaseURLType/syncModelAccount` |
| `POST /apps/installed/op` | `feishuTransition` |
| `POST /apps/installed/params/update` | `gpu/modelAccountBaseURL/modelAccountBaseURLType/syncModelAccount` |
| `POST /cronjobs` | `alertTriggerMode` |
| `POST /cronjobs/update` | `alertTriggerMode` |
| `POST /files/wget` | `useServerFilename` |
| `POST /ai/gpu/search` | `aggregation/deviceID/legacy` |
| `POST /runtimes/update` | `appDetailId` |

当前严格输入 Schema 会拒绝这些未声明字段。升级目标面板后，应导出其实际 Swagger 并复核差异。
响应目前校验 HTTP 状态、1Panel 状态包装、JSON 格式和大小，不逐个验证所有返回 DTO 的业务字段。
部分 Swagger 写接口没有请求 DTO，只能转发显式 JSON body，具体业务字段仍由面板验证。

## 验证记录

- `python scripts/generate-api-tools.py --check`：生成结果一致。
- `python scripts/test-api-catalog.py`：全部参数/引用约束、生成纯函数行为及异常契约检查通过。
- `go test ./internal/legacy ./internal/panel ./internal/toolset/...`：通过。
- `go test ./... -skip '^TestEnsureTLSMaterial$'`：通过。
- `go vet ./...`：通过。
- `go test ./...`：原有 `TestEnsureTLSMaterial` 在 Windows 上报告 `0666` 与预期 POSIX `0600` 不符；未修改或掩盖该测试。
- 本地 Postman simulation：创建后查询、提交后返回 500 的两种场景通过；日志为 200/200/500/200，共 4 次请求，未重试写入。
- 无真实面板的批量业务调用，未执行真实删除、安装或设置修改。模拟只覆盖两个文件接口，不代表全量接口验收。

后续验收需固定目标面板版本、确认 Core 的 endpoint-v1 能力，并在隔离实例上验证各类别的读写、上传下载和会话流程。
