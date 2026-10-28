# AtomCode2api (Go)

![Go](https://img.shields.io/badge/Go-1.24-00ADD8?logo=go&logoColor=white) ![Platform](https://img.shields.io/badge/platform-Windows%20%7C%20Linux-lightgrey) ![Type](https://img.shields.io/badge/type-API%20Gateway-blue)

AtomCode 转 OpenAI / Anthropic 兼容 API 的 Go 网关实现。

## 构建

```bash
go build -o atomcode2api .
```

## 运行

```bash
./atomcode2api -auth ~/.atomcode/auth.toml
```

| 参数/环境变量 | 说明 |
|---|---|
| `-auth` | 凭据文件路径，默认 `~/.atomcode/auth.toml` |
| `PORT` / `HOST` | 监听地址，默认本地端口启动时打印 |
| `API_KEY` | 客户端鉴权密钥 |
| `DUMP` | 请求转储开关，`0` 关闭 |
| `DUMP_DIR` | 转储目录 |

## 接口

- `GET /health` — 健康检查
- `GET /status` — 套餐状态（套餐名 / 5h 窗口水位 / 到期时间）
- `GET /v1/models` — 模型列表
- `POST /v1/chat/completions` — OpenAI Chat 兼容
- `POST /v1/messages` — Anthropic Messages 兼容
- `POST /v1/responses` — OpenAI Responses 兼容
- `GET /login`、`GET /login/poll` — 凭据登录与轮询
- `GET /admin` — Web 管理面板（内置，开箱即用）：凭据状态、套餐与 5h 窗口水位、模型列表、健康检查与令牌刷新。状态目录放一份自定义 `panel.html` 可覆盖内置版

`/admin/api/*` 面板后端：`overview`（凭据 + 套餐 + 模型概览）/ `refresh-token`（强制刷新当前凭据并写回 auth.toml）。

## 签名算法

### 概览

AtomGit LLM 网关（`llm-api.atomgit.com`）对每个请求强制 `X-AtomCode-*` 签名：
**HKDF-SHA256（Extract + Expand 单块）派生签名密钥，HMAC-SHA256 计算签名**。
已逐字节还原并通过真实抓包验证（2026-10-28，ts=1793159448，签名 `6a828f22…fb98c` MATCH）。
完整推导见 [`../docs/atomcode-signing-v1.md`](../docs/atomcode-signing-v1.md)。

### 输入

| 输入 | 说明 |
|---|---|
| method | 固定 `POST` |
| path | canonical 化：`{base_url_path}/chat/completions` |
| body | 线上原始字节（紧凑 JSON，无重序列化） |
| oauth_token | auth.toml 的 `access_token` |
| user_id | auth.toml 的 `[user].id` |
| timestamp_unix | 秒级 Unix 时间戳 |
| nonce | 16 字节随机数，传输为 hex |
| client_version | 客户端版本串（`X-AtomCode-Ver`，如 `5.2.4`） |

### 步骤

```python
# 1. salt：userID || 0x01 || uint64_le(ts/3600) || sha256(token) || sha256(version)
salt = user_id.encode() + b"\x01"
salt += (timestamp // 3600).to_bytes(8, "little")      # 按小时分桶（关键：字典不可猜）
salt += hashlib.sha256(oauth_token.encode()).digest()
salt += hashlib.sha256(client_version.encode()).digest()

# 2. HKDF-Extract
prk = hmac_sha256(key=salt, data=master_key)

# 3. HKDF-Expand（单块，info = "atomcode-signing-v1" || 0x01）
signing_key = hmac_sha256(key=prk, data=b"atomcode-signing-v1" + b"\x01")

# 4. 待签串（\n 连接，顺序固定：版本、方法、路径、时间戳、nonce、body哈希）
canonical = "\n".join([
    "v1", method.upper(), path, str(timestamp), nonce_hex,
    hashlib.sha256(body).hexdigest(),
])

# 5. 签名
sig = hmac_sha256(key=signing_key, data=canonical).hexdigest()   # 64 hex
```

### masterKey（硬编码常量，32 字节）

```
e97250f05303162c8ecd68c688b2f55c1d81e508d243d88466472e7f54637123
```

### 输出头

```
X-AtomCode-Sig:   v1:{sig}            # 64 hex
X-AtomCode-Ts:    {timestamp_unix}
X-AtomCode-Nonce: {nonce hex}         # 16 字节
X-AtomCode-Alg:   1
X-AtomCode-Ver:   {client_version}
Authorization: Bearer {oauth_token}   # 签名与鉴权分离
```

### 关键注意点

1. **时间桶**：`timestamp/3600` 整除向下取整进 salt——同一小时内签名密钥相同，跨小时轮换；桶边界附近重算即可。
2. **body 必须用线上原始字节**：JSON 重序列化（键序、空白、`ensure_ascii` 策略）任何差异都会改变 sha256。
3. **版本串进 salt**：`--ver` 必须与实际客户端版本一致（`5.2.3` 和 `5.2.4` 密钥不同）。
4. **上游可能轮换**：masterKey 或 salt 结构变化时，重新抓一次请求按上法验证即可定位差异。

### 使用

```bash
# Python 签名器（每个请求产出 5 个头）
python ../signing/atomgit_sign.py --body <req.json> --token <token> --user-id <uid>

# 端到端验证
python ../signing/verify_sign.py
```

Go 参考实现见 [`../signing/signing_ref.go`](../signing/signing_ref.go)（std 库实现，含 fixture 测试）。

### 端点事实

- `llm-api.atomgit.com`（官方默认端点）：**强制**签名，缺签名一律 403（此前「任意 UA 均 403、网关不可用」系缺签名所致，已作废）。
- `api-ai.gitcode.com`（本网关实际走的端点）：实测**不强制**签名，仅禁止 `atomcode/*` UA。
- 本网关走 `api-ai.gitcode.com + 中性 UA`，故无 signer 模块（凭据装载见 `internal/lanes/auth.go`）。

## Docker 部署

```bash
# 配置环境变量后启动
cp .env.example .env   # 按需填写
docker compose up -d
```

镜像同时发布到 GHCR：`ghcr.io/hj01857655/atomcode2api`（跟随 v* 标签发布，含 latest，linux/amd64 + linux/arm64）。

二进制发版：推送 `v*` 标签后 Actions 自动构建 linux / windows / darwin 的 amd64 + arm64 六平台产物并发布 Release。
