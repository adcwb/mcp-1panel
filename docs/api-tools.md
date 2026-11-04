# 全量 API 工具

项目以根目录 `doc.json` 为固定接口契约，新增 **770 个 API 工具、58 个接口分类**，
并保留原有 20 个便捷工具。接口清单见 [api-coverage.md](api-coverage.md)。
这表示文档中的每个 HTTP method/path 均有对应工具，不表示已在真实面板上逐项验收。
定义核对、已修复问题及尚未验证的能力见 [接口审查记录](api-audit.md)。
文档的 `info.version` 是 `2.0`，并未标明面板发行版本；实际可调用范围仍取决于目标面板版本、
已安装组件、许可证及调用身份的权限。浏览器登录、WebAuthn 等流程仍需调用方提供相应输入；
MCP 不会创建共享 Cookie 会话或绕过面板的认证要求。

## 启用与分类

独立模式使用 `--access-level=full`（或 `MCP_ACCESS_LEVEL=full`）开放新增工具。
`readonly` 仍为原有 15 个查询工具，`readwrite` 仍为原有 18 个工具，`full` 共 790 个工具。
新增工具可读取文件、密钥、应用环境及数据库配置，也可执行删除、安装、系统设置等操作，
因此统一采用 `privileged` 风险级别，不自动增加低权限凭据的能力。

工具按 Swagger tag 分类，例如 File、Container、AI、Database PostgreSQL 和 Firewall。
`tools/list` 的 `_meta["1panel/category"]` 以及 `--catalog` 都提供分类 ID、名称和排序。
分类只是展示信息，授权仍检查具体工具和节点。客户端应按 MCP 的 `nextCursor` 读取分页目录。

```sh
# API Key 和地址使用现有 PANEL_ACCESS_TOKEN、PANEL_HOST 环境变量。
mcp-1panel --mode=legacy --transport=stdio --access-level=full

# 不连接面板，导出全部工具及输入 Schema。
mcp-1panel --catalog > catalog.json
```

## 命名与参数

工具名为 `api_<method>_<path>`，动态路径段使用 `by_`；名称超过 64 字符时附加稳定摘要。
每个工具固定绑定一个接口，不接受自定义 URL、认证头、方法或路径模板。
顶层参数按位置组织：`path`、`query`、`body`、`form`、`headers`、`stream`，以及可选 `nodeId`。
只有该接口声明的参数组会出现在 Schema 中。DTO 必填字段、枚举、嵌套结构、映射和递归引用均参与校验。
Swagger 未声明请求 DTO 的写接口提供可选 `body`，具体业务字段由面板验证，不虚构字段契约。

```json
{
  "name": "api_post_containers_search",
  "arguments": {
    "nodeId": "local",
    "body": {"page": 1, "pageSize": 20, "order": "null", "orderBy": "name", "state": "all"}
  }
}
```

完整必填字段以该工具的 `inputSchema` 为准。路径参数示例：

```json
{"name":"api_get_apps_by_key","arguments":{"path":{"key":"mysql"}}}
```

`nodeId` 继续使用 `list_nodes` 返回的稳定 ID；独立模式每次解析当前节点名称，失败不会回落到主节点。
Core 自身的接口要求 `nodeId=local`；声明 `operateNode` 的接口由传输层从 `nodeId` 派生该参数。
用户不能通过 query/header 覆盖节点身份或认证信息。

## 文件与流

上传文件放入 `form`，每个文件包含 `filename` 和 base64 编码的 `data`。
不读取 MCP 进程所在机器的任意文件路径。

```json
{
  "name":"api_post_files_upload",
  "arguments":{"form":{"path":"/tmp","overwrite":false,"file":{"filename":"hello.txt","data":"aGVsbG8="}}}
}
```

下载结果提供 `encoding:base64`、`data`、`size`、`filename`、`contentType` 和 `contentRange`。
单次 API 原始响应上限 2 MiB；较大文件使用 `/files/chunkdownload` 的 `headers.Range` 分段读取。
Range 格式为 `bytes=start-end`，每段不超过 2 MiB。请求参数上限 1 MiB，文件 base64 字段最多
900,000 字符；较大上传使用 `/files/chunkupload`，文件字段实际名为 `chunk`。
最终 JSON 结果上限 4 MiB；超过限制返回错误，不静默截断。

```json
{
  "name":"api_post_files_chunkdownload",
  "arguments":{"body":{"path":"/tmp/data.bin","name":"data.bin"},"headers":{"Range":"bytes=0-1048575"}}
}
```

终端、脚本运行、进程和下载进度接口使用 WebSocket。每次工具调用建立一个独立连接，
发送 `stream.messages`，读取输出后关闭。帧类型为 `text` 或 `binary`；二进制帧内容使用 base64。
文本帧必须使用对应面板端点的消息协议，不能把任意终端字符串当作所有端点通用的帧格式。
这提供限时交互能力，不提供跨工具调用的持久终端会话。
固定版本后端的终端及会话管理接口还要求面板签发的用户、认证会话及有效期身份；
独立模式只发送 API Key 身份，不伪造这些内部头，因此不能保证终端可用。
返回的 `requestStatus:accepted` 仅表示传输请求已接收，不能作为命令执行成功的证明；需检查实际帧内容。

容器日志 `/containers/search/log` 使用 SSE，返回解析后的 `events`。
两种流均支持 `stream.readTimeoutMs`（默认 1000，最大 30000）和 `stream.maxMessages`
（默认 20，最大 100），返回 `stopReason`（`closed`、`deadline` 或 `message_limit`）。
达到读取限制只表示本次捕获结束，不表示远程任务完成。

JSON 响应保留对象、数组、标量和 `null`；仅在原有便捷工具中继续执行字段投影。
空成功响应返回 `requestStatus:accepted`。异步任务需另行查询状态。
写操作提交后的连接失败、5xx 或无效响应按“结果未知”处理，不自动重试。

## 托管模式与 endpoint-v1

新增工具使用 `schemaVersion:3`，operation 增加 `transport` 字段，记录 `kind`、`encoding`、
`fileFields`、`nodeQuery` 和可选 `response`。该字段纳入现有 policy hash；原有工具的序列化和 hash 不变。
原有运行时 contract-v5 握手保持兼容，但 **Core/Agent 必须实现 endpoint-v1 才能执行新增工具**。
此仓库仅实现 MCP 适配器，不包含 Core/Agent 的调度器源码，无法替代它们的升级。

Core 实现要求：

1. 导入工具目录，校验完整 JSON Schema，包括本地引用和 map；将新工具作为显式的高权限授权。
2. 将 `Invocation.arguments` 解释为 `path/query/body/form/headers/stream` 参数组；
   `bodySchema` 校验的是这些参数组，不是直接发送到 HTTP 的 DTO。
3. 从已批准的 operation 取得 HTTP 方法、路径和编码；校验 `pathParams` 与参数一致，
   使用准入票据中的节点和身份，禁止调用方覆盖认证头或路由。
4. 对 multipart、二进制、WebSocket 和 SSE 实施与本文一致的边界；新工具结果按显式授权返回，
   原有工具继续执行字段投影。目录响应允许 32 MiB，业务结果仍有独立大小限制。
5. 保留每次调用的准入、权限复核和审计；不支持扩展时不发布该工具的授权。

适配器只接受与本地定义完全一致的授权目录和 policy hash。旧 Core 丢弃 `transport` 后得到的
hash 不匹配，会拒绝调用；不会偷偷改用独立模式 API Key。

## 更新与验证

```sh
python scripts/generate-api-tools.py
python scripts/generate-api-tools.py --check
python scripts/test-api-catalog.py
go test ./...
go vet ./...
```

生成结果编译进二进制，运行时无需 `doc.json`，也不在线下载 Swagger。
`scripts/api-overrides.json` 保存已核实的文档修正及官方源码链接，例如上传目录参数、
分块上传的 `chunk` 字段、分块下载的错误 DTO，以及缺失的 SSE/WebSocket 标记。
更新面板文档后应复核这些修正；生成器遇到失效的修正或未解析引用会报错。

Go 测试通过本地 HTTP/WebSocket/SSE 服务检查认证、路由、参数、文件和流编码、错误与权限边界；
覆盖清单测试逐项比较原始 Swagger，Python 契约测试还核对全部声明参数及引用定义的约束是否保留。
普通 HTTP API 必须返回有效的 1Panel `code` 包装；空响应、无状态码的 JSON 和异常文本不会被当作成功。
文件、导出及图片端点由修正清单显式声明二进制响应，空文件与带 `code` 字段的 JSON 文件仍保留原始字节。
数值输入保留精度，64 位整数不会经 `float64` 转换后再发送；整数形式的 `1.0/1e0` 规范化为 `1`，
以匹配 Go DTO 解码。运行时数值校验有大小边界。
这些测试不能证明真实面板的所有业务操作都成功，尤其不能替代
托管 Core 升级和部署后联调。Windows 上原有 `TestEnsureTLSMaterial` 依赖 POSIX 0600 权限位，
完整测试需在支持该权限模型的系统运行。

本地 Postman 模拟服务覆盖“创建后查询”及“创建已提交但返回 500”的场景。无需登录或连接真实面板：

```sh
postman simulation run postman/simulations/1panel/1panel.sim.yaml --port 47861 --output ndjson --no-history
# 在另一个终端执行；Windows PowerShell 使用 $env:MCP_TEST_SIMULATION_URL='http://127.0.0.1:47861/panel'
MCP_TEST_SIMULATION_URL=http://127.0.0.1:47861/panel go test ./internal/legacy -run TestEndpointPostmanSimulation -v
```

该测试使用固定的测试密钥，并限制目标为本机回环地址。模拟服务仅实现两个文件接口，其余返回 501。
CLI 不支持 `postman status`，因此未搜索远端团队 mock；本地模拟依据用户提供的契约与官方源码构建。
本次模拟测试通过，日志确认错误后未重试写入；结果不等同于真实面板业务验收。
