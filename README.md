# Atom2Api

English | [简体中文](./README.zh-CN.md)

Atom2Api exposes AtomGit Coding Plan accounts through an OpenAI-compatible API for external applications. It also provides dashboards for accounts, quotas, API keys, model routing, and token usage.

## What This Fork Improves

This repository is maintained by [lwang998](https://github.com/lwang998/Atom2Api). On top of the original project it adds:

- **Desktop double-click experience**: a bundled Tauri shell (`desktop/`). Unzip the release, double-click `Atom2Api.exe`, and the service starts with the console window opened; closing the window stops everything. The standalone server also opens the console in your browser automatically.
- **Zero configuration**: config and the database live in a hidden `.atom2api` folder under your user profile (hidden attribute applied on Windows), generated on first launch — replacing the executable never requires migrating data.
- **No login on localhost**: the admin console opens without a password when accessed from the same machine; LAN/remote access still requires the password, and `ATOM2API_REQUIRE_LOGIN=1` forces password login everywhere (recommended behind a reverse proxy).
- **Fixes an upstream tool-call bug**: parameterized tools failing with `required parameter missing` in Claude Code and similar clients (streaming `tool_use` argument fragments were dropped) — see "Tool call fixes (2026-10-02)" below.
- **Anthropic-compatible endpoints**: `/v1/messages` and `/v1/messages/count_tokens` for direct Claude Code / Cherry Studio integration.
- **Update checks point to this fork**: the console's version check and changelog follow this repository's releases.

## Security Warning

> [!WARNING]
> This project is intended for use only in trusted network environments. Before deploying or using it, carefully assess the following security risks:

- **Plain HTTP by default**: Atom2Api does not provide TLS. Prompts, source code, responses, and API keys may be transmitted in plain text between clients and the service. The default listen address, `:8080`, may also expose the service to other devices on the same network. For anything beyond local-only use, you must configure an HTTPS reverse proxy, a firewall, and strict access controls.
- **Insecure default administrator password**: The initial administrator password is `atom2api`. Although the OpenAI-compatible API requires a generated API key, anyone who can reach the service and knows the default password may access the management console. Change the password immediately after the first login and protect all issued API keys.
- **Sensitive local data**: OAuth tokens are encrypted at rest with AES-256-GCM, API keys are stored as SHA-256 digests, and the administrator password is stored as a bcrypt hash. However, `config.json` still contains the `encryption_key` and optional `signer_token`. Any process or user with access to both `config.json` and `data/` can decrypt the OAuth credentials. Restrict access to these files and back them up together securely.
- **Audit logs may contain request content**: The SQLite database records request and response bodies when audit debug mode is enabled, and always records upstream error responses. These records may contain prompts, source code, personal data, or other secrets. Restrict database access, limit backup scope, and clean up records according to your data-retention requirements.
- **Unofficial compatibility implementation**: This project accesses the AtomGit upstream through an independently documented, community-maintained signing implementation. It is not an official AtomGit/AtomCode client or supported integration. Upstream protocol changes may break the service at any time, and usage may result in account restrictions or bans.

**Disclaimer**: This project is for learning and research only. It is not affiliated with or endorsed by AtomGit/AtomCode. You are responsible for evaluating the risks, complying with the AtomGit/AtomCode terms of service and applicable law, and accepting responsibility for account bans, data exposure, charges, or other losses.

**AI-generated code notice**: 100% of this project's code was written by AI (AtomCode / OpenCode). Humans only provided requirements and reviewed the result.

## Features

- AtomGit broker OAuth: starts authorization, polls status, exchanges tokens, and automatically refreshes them five minutes before expiry
- Coding Plan: claims plans in `Max -> Pro -> Lite` order and reads subscription type, rolling quota, expiry time, model catalog, and 60-day usage
- OpenAI-compatible endpoints: `/v1/models`, `/v1/chat/completions`, `/v1/responses`, `/v1/completions`, and `/v1/embeddings`
- Anthropic-compatible endpoints: `/v1/messages` and `/v1/messages/count_tokens` for direct Claude Code, Cherry Studio, and other Anthropic-protocol clients (see below)
- Desktop double-click experience: Tauri shell + server in a single package, data in a hidden `~/.atom2api` folder, no login on localhost
- Streaming proxy: forwards SSE in real time, automatically requests `include_usage`, and records input, output, cached, and reasoning tokens
- Multi-account routing: supports per-request random routing or sticky per-key, per-model account filling
- Account credential migration: exports an OAuth credential bundle from Accounts and imports it on another device before synchronizing the account
- API keys: stores SHA-256 digests only and supports model allowlists, revocation, restoration, and expiration
- Management security: bcrypt password hashing, HttpOnly and SameSite sessions, login rate limiting, and AES-256-GCM encryption for stored OAuth tokens
- Dashboard: request trends, request counts, input/output tokens, success rate, latency, model distribution, and recent requests
- Single-binary deployment: embeds the React console in the Go service and also provides Docker and Compose configurations

### Experimental Responses API Compatibility Conversion

Model management provides an experimental "Responses to Chat" switch. When disabled, `/v1/responses` requests are forwarded unchanged to the model's native Responses endpoint. When enabled, Atom2Api calls `/v1/chat/completions` directly and converts the request, buffered response, and streaming SSE back to the Responses API. The switch defaults to enabled for `GLM-5.2` and disabled for other models; models with a verified native Responses channel should keep it disabled. The response header `X-Atom2api-Responses-Compat: chat-completions` identifies requests served by this conversion.

The compatibility layer supports text, image URLs, function tools, JSON Schema, usage, and streaming events. It accepts `prompt_cache_key` but does not forward this cache-routing hint to Chat Completions. Chat Completions cannot provide equivalent server-side state such as `store=true` or `previous_response_id`, or OpenAI built-in tools; those inputs return `400 unsupported_parameter` instead of being silently ignored.

### API key routing strategies

Each API key can choose **Fill** or **Round robin** in Key Management:

- **Fill** keeps a key on one account for each model and persists the binding. It switches only when the account is disabled, invalid, unavailable for that model, or its five-hour quota is exhausted. Different keys are assigned different accounts when possible, and share an account only when there are not enough eligible accounts.
- **Round robin** randomly selects an eligible account for every request and does not use bindings.

Plan-aware routing keeps Lite accounts on DeepSeek. DeepSeek prefers non-exhausted Lite accounts and falls back to Pro/Max when Lite is unavailable. GLM-5.2 is restricted to Pro, Pro trial, and Max accounts. Fill bindings and the selected account's five-hour call quota are visible in Key Management.

Account management also provides protocol probing for every available model on an account. The probe checks Chat Completions and native Responses separately. It uses normal JSON requests by default and can optionally validate streaming SSE completion events.

## Local Development

Requires Go 1.22+ and Node.js 20+.

```bash
cd frontend
npm ci
npm run build
cd ..
go test ./...
go run .
```

Open `http://localhost:8080`. On first launch, Atom2Api creates `config.json` with a random `encryption_key` and a bcrypt hash of the default administrator password `atom2api`; change the password immediately under **System Settings** after logging in. Plaintext passwords entered through settings or a configuration reload are automatically replaced with bcrypt hashes.

You can also start with the example configuration:

```bash
cp config.example.json config.json
go run .
```

The equivalent Windows PowerShell commands are:

```powershell
Copy-Item config.example.json config.json
go run .
```

## Docker

The Compose configuration uses the published [`cnluminous/atom2api:latest`](https://hub.docker.com/r/cnluminous/atom2api) image from Docker Hub, so no local image build is required.

```bash
docker compose pull
docker compose up -d
```

Open `http://localhost:8080` after the container starts. Data is stored in the local `data/` directory. After the first launch, open the console and change the default password.

## Usage

1. Sign in to the console and open **Accounts**.
2. Select **Connect AtomGit** and complete OAuth authorization on the page that opens.
3. Atom2Api automatically claims or synchronizes the Coding Plan, quota, and available models.
4. To move an account to another device, export the credential JSON from Accounts and import it on the target device; the account is matched by user ID and synchronized again.
5. Open **API Keys** and create an API key for external clients. The plaintext key is displayed only once.
6. Set the client's base URL to `http://localhost:8080/v1` and use the newly created `sk-atom2-*` API key.

The exported credential file contains OAuth access tokens. Transfer it only through a secure channel, protect it like a password, and delete temporary copies after import.

Python SDK example:

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

Streaming request:

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-atom2-your-key" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","stream":true,"messages":[{"role":"user","content":"hello"}]}'
```

## Anthropic (Claude) API

Atom2Api also exposes an Anthropic Messages API compatible endpoint, so Claude Code, Cherry Studio (Anthropic channel), and other Anthropic-protocol clients work unchanged:

- **Base URL**: `http://localhost:8080` (clients append `/v1/messages` automatically)
- **API Key**: the same `sk-atom2-*` key used by the OpenAI endpoints; both `x-api-key` and `Authorization: Bearer` headers are accepted
- **Models**: the same list as shown in model management

curl example:

```bash
curl http://localhost:8080/v1/messages \
  -H "x-api-key: sk-atom2-your-key" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-flash","max_tokens":1024,"messages":[{"role":"user","content":"hello"}]}'
```

Requests, responses, and streaming SSE are fully translated to the Anthropic protocol: `stop_reason` is mapped automatically (`length` → `max_tokens`, `tool_calls` → `tool_use`, otherwise `end_turn`), reasoning content is returned as `thinking` blocks, and tool calls map to `tool_use`/`tool_result` blocks. Errors use the standard Anthropic envelope; upstream overload (529/503) returns `overloaded_error`, and no available account returns 429. `POST /v1/messages/count_tokens` estimates the input token count at roughly 4 characters per token without contacting the upstream.

Claude Code example (other Anthropic-protocol clients work the same way — point them at the same Base URL and key):

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_AUTH_TOKEN=sk-atom2-your-key
claude --model deepseek-v4-flash
```

### Tool call fixes (2026-10-02)

- **Streaming `tool_use` parameter fix**: upstream OpenAI streaming `tool_calls` fragments are now buffered by `index` and emitted once the stream completes as `content_block_start` / `input_json_delta` / `content_block_stop`, so parameter JSON is no longer dropped or interleaved; parallel tool calls keep their arguments separate, and parameterized tools (shell commands, file reads/writes) work again in Claude Code / Paseo. The previous symptom was every parameterized tool failing with `required parameter missing` while parameter-free tools worked. Commit `ccfc03b`.
- **Tool schema fix**: tool definitions sent upstream now prefer the Anthropic `input_schema`, falling back to an empty object schema when absent. Commit `ccfc03b`.
- **Non-streaming parameter mapping hardening**: the OpenAI `arguments` string maps correctly to the Anthropic `input` object; when upstream returns malformed JSON, the response preserves the raw text as `{"_raw_arguments": "..."}` instead of silently substituting an empty object (so "missing parameter" reports are no longer untraceable). Commit `52ce32f`.
- **Related fixes**: `/v1/models` filters by the API key's model allowlist and tolerates the `[1m]` context suffix (`ccfc03b`, `f030e59`); streaming request audit records now capture first-token and completion latency (`1fee2e3`).

The fixes above ship in builds `1.0.10+52ce32f` and later (visible in the `version` field of `GET /api/health`). Regression tests cover interleaved streaming fragments, non-streaming mapping, and malformed-argument evidence; see `anthropic_test.go` and `anthropic_stream_test.go`, verified with `go test ./...`.

## Configuration

| Field | Default | Description |
|---|---|---|
| `listen_address` | `:8080` | HTTP listen address; the `PORT` environment variable takes precedence |
| `data_path` | `data/atom2api.db` | SQLite database containing accounts, API key digests, settings, counters, and audit records |
| `admin_password` | bcrypt hash of `atom2api` | Management console password; plaintext values are automatically hashed and rewritten |
| `encryption_key` | Randomly generated on first launch | OAuth token encryption key; do not change or disclose it |
| `platform_base_url` | `https://acs.atomgit.com` | OAuth broker |
| `codingplan_api_url` | `https://api.gitcode.com/api/v5` | Coding Plan REST API |
| `gateway_url` | `https://llm-api.atomgit.com/v1` | Default LLM gateway |
| `signer_url` | Empty | Optional external signing service; the built-in signer is used when empty |
| `audit_debug_enabled` | `false` | Records full request/response bodies and sanitized headers when enabled |
| `audit_retention_days` | `30` | Default retention period used when manually deleting complete audit records |
| `audit_detail_retention_days` | `30` | Default retention period used when manually clearing audit bodies, headers, errors, and retry details |
| `request_timeout_seconds` | `600` | Upstream request timeout, from 5 to 600 seconds |
| `request_retry_count` | `3` | Number of retries for matching upstream HTTP statuses, from 0 to 10; 0 disables retries |
| `request_retry_status_codes` | `429` | Comma-separated statuses and ranges, such as `400-500,503,429`; overlapping and adjacent values are merged automatically |

All runtime state is stored in the SQLite database at `data_path`, including accounts, API keys, model settings, plan claim logs, and the latest 50,000 request records used for dashboard aggregation. On startup, an existing legacy JSON state file and its `<data_path>.usage.ndjson` log are migrated transactionally into the database and removed only after the migration commits. By default only request metadata and usage are retained. Full bodies and headers are stored only when audit debug mode is enabled. Upstream responses with a non-200 status are retained regardless of that setting. Retries stay in the original audit record, which reports the retry count and retains every attempt's response body and sanitized headers. Authentication, cookie, token, API key, and signature header values are redacted.

## Signing Compatibility

The AtomGit LLM gateway currently requires `X-AtomCode-*` request signatures. The open-source AtomCode repository exposes only the signer interface; the implementation included in official builds is not public. Atom2Api includes an independently documented, community-maintained implementation of `atomcode-signing-v1`, with separate fixtures that verify the HKDF/HMAC output. See [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for attribution.

If the upstream protocol or key changes, configure `signer_url` in the settings to override the built-in implementation. The signing service receives JSON in this format:

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

It must return `{"headers":{"X-AtomCode-Sig":"..."}}`. An external signer receives the OAuth token, so configure only a trusted HTTPS service or a local loopback address.

## Security and Operations

- Do not commit `config.json`, `data/`, or API keys. These paths are included in `.gitignore`.
- Any public deployment must sit behind an HTTPS reverse proxy and use a password other than the default administrator password.
- Back up `config.json` and `data/` together. Existing OAuth tokens cannot be decrypted if the `encryption_key` is lost.
- This is not an official AtomGit/AtomCode project. Before using it, confirm that your request patterns comply with your account plan and the applicable terms of service.

## Verification

```bash
go test -count=1 -cover ./...
go vet ./...
cd frontend && npm run build
```

Run the live non-streaming, streaming, and function-tool Responses checks against `GLM-5.2` with the account in the current `config.json`:

```bash
ATOM2API_LIVE_GLM=1 go test -run TestManualGLMResponsesChatCompatibility -count=1 -v
```

Probe the Chat and native Responses protocols for every available model on all configured accounts. Add `ATOM2API_LIVE_PROBE_STREAM=1` to use streaming SSE probes:

```bash
ATOM2API_LIVE_PROBE=1 go test -run TestManualConfiguredAccountProtocols -count=1 -v
```

The test suite covers credential encryption, API key digests, Coding Plan claim fallback and synchronization, signing fixtures, non-streaming and streaming proxy usage, server-side management sessions, and live configuration reloads.

## Acknowledgements

Our sincere thanks to every member of the [Linux.do](https://linux.do/) community. Your sincerity, kindness, solidarity, and professionalism are what make this community so vibrant.
