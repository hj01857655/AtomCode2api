# AtomCode2api 凭据与上游

## 凭据

- 登录来源：`~/.atomcode/auth.toml`（官方 `atomcode login` 产出）或网关 `GET /login` 自建 OAuth。
- 网关独立凭据文件：`<状态目录>/auth.json`（0600 原子写）。加载优先级：auth.json 存在优先，否则读 auth.toml 迁移。
- 刷新产物（新 AT/RT）只落 auth.json，不再回写 auth.toml（RT 轮换吊销语义，写回会让官方 CLI 旧 RT 失效）。
- auth.toml 保留在原处作为首次迁移源；官方 CLI 掉线时重登一次即自愈。

## 上游

- 账号面/套餐：atomgit 域；模型：CodingPlan（qwen3.8-27b、glm5.3-flash）+ OpenRouter 免费模型。
- 默认端口 8788（README 旧版写 8576 是错的）。
- 自动领取：启动时检查/领取 CodingPlan 体验版（日志 [claim]）。
