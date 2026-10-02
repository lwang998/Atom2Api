# Atom2Api

[English](./README.md) | 简体中文

Atom2Api 将 AtomGit Coding Plan 账号统一转换为可供外部应用调用的 OpenAI 兼容接口，并提供账号、额度、密钥、模型路由和 Token 用量仪表盘。

## 安全警告

> [!WARNING]
> 本项目仅适合在可信网络环境中使用。部署或使用前，请充分评估以下安全风险：

- **默认使用明文 HTTP**：Atom2Api 本身不提供 TLS。客户端与服务之间的提示词、代码、响应和 API Key 均可能以明文传输；默认监听地址 `:8080` 还可能使服务暴露给同一网络中的其他设备。非纯本机使用时，必须配置 HTTPS 反向代理、防火墙和严格的访问范围。
- **默认管理密码不安全**：首次启动的管理密码为 `atom2api`。OpenAI 兼容接口虽要求使用已创建的 API Key，但任何能访问服务且知道默认密码的人都可能进入管理控制台。首次登录后必须立即修改密码，并妥善保管已签发的 API Key。
- **本地数据包含敏感信息**：OAuth token 使用 AES-256-GCM 加密保存，API Key 仅保存 SHA-256 摘要，管理密码仅保存 bcrypt 哈希；但 `config.json` 仍包含 `encryption_key` 及可选的 `signer_token`。同时取得 `config.json` 和 `data/` 的进程或用户可以解密 OAuth 凭据，请严格限制这些文件的读取权限并一同安全备份。
- **审计日志可能保存请求内容**：开启审计调试后，SQLite 数据库会记录请求与响应正文；上游错误响应则始终记录。其中可能包含提示词、源代码、个人信息或其它秘密，请限制数据库访问、控制备份范围，并按数据保留要求及时清理。
- **依赖非官方兼容实现**：本项目通过社区独立记录的签名兼容实现访问 AtomGit 上游，不属于 AtomGit/AtomCode 官方客户端或受支持集成。上游协议变更可能随时导致服务不可用，使用方式也可能带来账号限制或封禁风险。

**免责声明**：本项目仅供学习和研究，不隶属于 AtomGit/AtomCode，也未获其官方认可。使用者需自行评估风险、遵守 AtomGit/AtomCode 的服务条款及适用法律，并对账号封禁、数据泄露、费用或其它损失承担全部责任。

**AI 生成说明**：本项目代码 100% 由 AI（AtomCode / OpenCode）编写，人类仅负责提出需求和审核。

## 已实现

- AtomGit broker OAuth：启动授权、轮询状态、交换 token、提前 5 分钟自动刷新
- Coding Plan：按 `Max -> Pro -> Lite` 领取，读取订阅类型、滚动额度、到期时间、模型目录和 60 天用量
- OpenAI 兼容端点：`/v1/models`、`/v1/chat/completions`、`/v1/responses`、`/v1/completions`、`/v1/embeddings`
- 流式代理：SSE 即时转发，自动请求 `include_usage`，记录输入、输出、缓存和推理 tokens
- 多账号路由：支持按请求随机轮询，或按 API Key + 模型填充固定账号
- 账号凭据迁移：从账号管理导出 OAuth 凭据包，在另一台设备导入并自动同步账号
- API Key：仅保存 SHA-256 摘要，支持模型白名单、撤销、恢复和过期时间
- 管理安全：bcrypt 密码哈希、HttpOnly + SameSite 会话、登录限速、OAuth token AES-256-GCM 加密落盘
- 仪表盘：请求趋势、请求数、输入/输出 tokens、成功率、延迟、模型分布和最近请求
- 单二进制部署：React 控制台嵌入 Go 服务，另附 Docker/Compose

### Responses API 实验性兼容转换

模型管理提供“Responses 转 Chat”实验性开关。开关关闭时，`/v1/responses` 请求会原样发送到模型的原生 Responses 上游；开关开启时，Atom2Api 会直接改调 `/v1/chat/completions`，并将请求、普通响应和流式 SSE 转换回 Responses API。`GLM-5.2` 默认开启该开关，其它模型默认关闭；确认原生 Responses 渠道可用的模型应保持关闭。响应头 `X-Atom2api-Responses-Compat: chat-completions` 表示本次请求使用了兼容转换。

兼容层支持文本、图片 URL、函数工具、JSON Schema、用量和流式事件。兼容层会接受 `prompt_cache_key`，但不会把这一缓存路由提示转发给 Chat Completions。Chat Completions 无法等价提供的服务端状态（例如 `store=true`、`previous_response_id`）和 OpenAI 内置工具会返回 `400 unsupported_parameter`，不会被静默忽略。

### API Key 路由策略

每个 API Key 可以在“密钥管理”中选择“填充”或“轮询”：

- **填充**：同一 Key 请求同一模型时固定使用同一账号，绑定关系会持久化；账号停用、失效或 5 小时额度耗尽后才切换。不同 Key 会优先分配到不同账号，账号数量不足时允许共享。
- **轮询**：每次请求从当前可用账号中随机选择，不使用固定绑定。

模型分流遵循套餐能力：Lite 账号只参与 DeepSeek，DeepSeek 优先使用未耗尽的 Lite，Lite 不可用后回退到 Pro/Max；GLM-5.2 仅从 Pro、Pro 体验版和 Max 账号中选择。填充模式下，密钥管理页面会显示每个模型的当前账号及其 5 小时调用额度。

账号管理中的协议探测会对该账号的全部可用模型分别测试 Chat Completions 与原生 Responses。探测面板默认执行普通 JSON 请求，也可以开启流式传输测试以验证 SSE 完成事件。

## 本地运行

要求 Go 1.22+、Node.js 20+。

```bash
cd frontend
npm ci
npm run build
cd ..
go test ./...
go run .
```

打开 `http://localhost:8080`。首次运行会生成 `config.json`、随机 `encryption_key` 和默认管理密码 `atom2api` 的 bcrypt 哈希，登录后应立即在“系统设置”中修改。通过设置页或配置热加载写入的明文密码会自动替换为 bcrypt 哈希。

也可以先使用示例配置：

```bash
cp config.example.json config.json
go run .
```

Windows PowerShell 对应命令：

```powershell
Copy-Item config.example.json config.json
go run .
```

## Docker

Compose 配置使用 Docker Hub 发布的 [`cnluminous/atom2api:latest`](https://hub.docker.com/r/cnluminous/atom2api) 镜像，无需在本地构建镜像。

```bash
docker compose pull
docker compose up -d
```

容器启动后访问 `http://localhost:8080`。数据保存在本地 `data/` 目录。首次启动后访问控制台修改默认密码。

## 使用流程

1. 登录控制台，进入“账号管理”。
2. 点击“连接 AtomGit”，在新页面完成 OAuth 授权。
3. Atom2Api 自动领取或同步 Coding Plan、额度和可用模型。
4. 如需迁移到另一台设备，在账号管理导出凭据 JSON 文件，并在目标设备导入；导入后会按用户 ID 更新账号并重新同步。
5. 进入“密钥管理”创建外部 API Key；密钥明文只显示一次。
6. 将客户端的 Base URL 设为 `http://localhost:8080/v1`，API Key 使用刚创建的 `sk-atom2-*`。

导出的凭据文件包含 OAuth 访问令牌，只应通过安全渠道传输并像密码一样保管；导入完成后建议立即删除临时文件。

Python SDK 示例：

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key="sk-atom2-your-key",
)

response = client.chat.completions.create(
    model="deepseek-v4-flash",
    messages=[{"role": "user", "content": "hello"}],
)
print(response.choices[0].message.content)
```

流式请求：

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-atom2-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

## 配置

| 字段 | 默认值 | 说明 |
|---|---|---|
| `listen_address` | `:8080` | HTTP 监听地址；`PORT` 环境变量优先 |
| `data_path` | `data/atom2api.db` | 保存账号、密钥摘要、设置、计数和审计记录的 SQLite 数据库 |
| `admin_password` | `atom2api` 的 bcrypt 哈希 | 控制台管理密码；明文值会自动哈希并回写 |
| `encryption_key` | 首次启动随机生成 | OAuth token 加密密钥，不应更换或泄露 |
| `platform_base_url` | `https://acs.atomgit.com` | OAuth broker |
| `codingplan_api_url` | `https://api.gitcode.com/api/v5` | Coding Plan REST API |
| `gateway_url` | `https://llm-api.atomgit.com/v1` | 默认 LLM 网关 |
| `signer_url` | 空 | 可选外部签名服务；为空时使用内置 signer |
| `audit_debug_enabled` | `false` | 开启后记录完整请求/响应正文及脱敏 Header |
| `audit_retention_days` | `30` | 手动清理完整请求审计记录时默认使用的保留天数 |
| `audit_detail_retention_days` | `30` | 手动清理请求/响应正文、Header、错误和重试详情时默认使用的保留天数 |
| `request_timeout_seconds` | `600` | 上游请求超时范围 5-600 秒 |
| `request_retry_count` | `3` | 命中指定上游 HTTP 状态码后的重试次数，范围 0-10；0 表示不重试 |
| `request_retry_status_codes` | `429` | 逗号分隔的状态码或范围，例如 `400-500,503,429`；重叠和相邻项会自动归并 |

所有运行状态均写入 `data_path` 指向的 SQLite 数据库，包括账号、API Key、模型设置、领取日志，以及用于仪表盘聚合的最近 50,000 条请求记录。启动时如发现旧 JSON 状态文件及其 `<data_path>.usage.ndjson` 日志，会在事务中自动迁入数据库，并仅在事务提交成功后删除旧文件。默认只记录请求元数据与用量；开启审计调试模式后才保存请求/响应正文和 Header。无论调试模式是否开启，上游返回非 200 状态时都会保存其响应正文和 Header。重试不会创建新的审计记录，原记录会显示重试次数，并保存每次尝试的响应正文和脱敏 Header。认证、Cookie、Token、API Key 及签名 Header 的值会被脱敏。

## 签名兼容性

当前 AtomGit LLM 网关要求 `X-AtomCode-*` 请求签名。AtomCode 开源仓库只公开 signer 接口，官方构建中的实现不在仓库内。Atom2Api 内置了社区独立记录的 `atomcode-signing-v1` 实现，并用独立 fixture 验证 HKDF/HMAC 输出；来源见 [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md)。

如果上游轮换协议或密钥，可在设置中配置 `signer_url` 覆盖内置实现。签名服务接收 JSON：

```json
{
  "method": "POST",
  "path": "/v1/chat/completions",
  "body": "base64-encoded-request-body",
  "oauth_token": "...",
  "user_id": "...",
  "timestamp_unix": 0,
  "nonce": "base64url-nonce",
  "client_version": "5.0.2"
}
```

返回 `{"headers":{"X-AtomCode-Sig":"..."}}`。外部 signer 会接触 OAuth token，只应配置为受信任的 HTTPS 服务或本机回环地址。

## 安全与运维

- 不要提交 `config.json`、`data/` 或 API Key；这些路径已加入 `.gitignore`。
- 公网部署必须置于 HTTPS 反向代理之后，并修改默认管理密码。
- 备份时必须同时保存 `config.json` 和 `data/`；丢失 `encryption_key` 后现有 OAuth token 无法解密。
- 本项目非 AtomGit/AtomCode 官方项目。使用前请确认你的调用方式符合账号套餐和服务条款。

## 验证

```bash
go test -count=1 -cover ./...
go vet ./...
cd frontend && npm run build
```

使用当前 `config.json` 中的账号对 `GLM-5.2` 执行 Responses 非流式、流式和函数工具实测：

```bash
ATOM2API_LIVE_GLM=1 go test -run TestManualGLMResponsesChatCompatibility -count=1 -v
```

对当前配置中所有账号的可用模型执行 Chat 与原生 Responses 协议探测；追加 `ATOM2API_LIVE_PROBE_STREAM=1` 可改为流式 SSE 探测：

```bash
ATOM2API_LIVE_PROBE=1 go test -run TestManualConfiguredAccountProtocols -count=1 -v
```

测试覆盖凭据加密、API Key 摘要、Coding Plan 领取级联与同步、签名 fixture、非流式/流式代理用量、服务端管理会话和配置热加载。

## 致谢

衷心感谢 [Linux.do](https://linux.do/) 社区的所有成员。是你们的真诚、友善、团结与专业，让这个社区始终充满活力。
