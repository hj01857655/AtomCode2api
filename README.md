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
- `GET /v1/models` — 模型列表
- `POST /v1/chat/completions` — OpenAI Chat 兼容
- `POST /v1/messages` — Anthropic Messages 兼容
- `POST /v1/responses` — OpenAI Responses 兼容
- `GET /login`、`GET /login/poll` — 凭据登录与轮询
