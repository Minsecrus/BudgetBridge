# BudgetBridge

将多个含代金券的阿里云账号聚合为一个高可用的 API 代理服务，同时兼容 OpenAI 和 Anthropic 格式，并自带用量 / 余额监控仪表盘。

## 功能

- 🤖 同时兼容 OpenAI（`/v1/chat/completions`）与 Anthropic（`/v1/messages`）：后者端到端翻译（流式、工具调用、`cache_control` 透传）
- 🔄 账号池智能调度：**亲和**（会话粘性 + 加权 Rendezvous 哈希，按会话首条消息稳定路由到同一账号以复用上游 prefix cache，并按余额分档加权、对 429 冷却账号线性退避）或**轮询**，二选一
- 🛡️ 429 限流自动冷却 60s 并换号重试；所有账号不可用时返回 503
- 💰 代金券余额监控（每 5 分钟经阿里云 BSS API 查询），低于 ¥3.00 自动停用
- 🔐 统一 API key 鉴权（首次启动自动生成 `sk-bb-…`，可在后台查看 / 轮换），同时兼容 OpenAI `Authorization` 与 Anthropic `x-api-key`
- 📊 用量统计仪表盘：请求量趋势、成功率、429 数、各账号分担比、余额历史（SQLite 持久化，滚动保留 7 天，支持 1h / 24h / 7d 窗口）
- 🔧 模型名称覆盖（`model_override`），透明替换每个请求中的模型 ID
- 🧪 账号连通性测试：单账号探测或全池并发探测，直接验证每个 key 是否可用
- 🌊 Stream 流式响应透传（含 Anthropic `content_block_*` 事件翻译）
- 🖥️ React 后台：账号增删改、启停、查余额、解冻、测试，以及统计可视化
- 🔒 管理后台登录保护（可选），明文密码首次启动自动 bcrypt 加密

## 快速开始

### 本地开发（推荐）

```powershell
# 1. 复制并编辑配置
copy config.yaml.example config.yaml
# 编辑 config.yaml，填入真实 api_key / ak_id / ak_secret

# 2. 一键启动（弹出两个终端窗口）
.\dev.ps1
# 前端: http://localhost:5173
# 后端: http://localhost:<config.yaml 中 listen 的端口，默认 :8080>
```

> 需要先安装 [air](https://github.com/air-verse/air) 实现后端热重载：
> `go install github.com/air-verse/air@latest`
> 未安装时 `dev.ps1` 自动回退到 `go run .`

> **单端口模式**：设置环境变量 `BB_DEV=1` 后，后端会反向代理前端到 vite dev server，此时只需访问后端端口即可同时用前端和 API（默认双端口模式下二者分离）。

### Docker 部署

```bash
cp config.yaml.example config.yaml
# 编辑 config.yaml，设置 listen 端口和其他配置

docker compose up -d
# 前端: http://localhost:<前端端口>
# API:  http://localhost:<config.yaml 中配置的端口>/v1
```

> `docker-compose.yml` 会把宿主机的 `config.yaml` 挂载进容器、并以 `./data:/app/data` 持久化统计数据库。

#### 交互式部署（推荐）

```bash
./scripts/deploy.sh
```

脚本会引导填写配置（包括端口和域名），可选自动配置 Caddy + HTTPS。所有配置会写入根目录的 `config.yaml`，并自动同步到 Docker 和 Caddy 配置。

#### 生产环境

- Caddy 配置：`caddy/Caddyfile`
- 详细指南：[部署文档](docs/deployment.md)

## API 接入

代理端点由**统一 API key** 保护。首次启动若 `config.yaml` 中 `api_key` 为空，后端会自动生成一个 `sk-bb-…` 并写回配置；可在管理后台查看与轮换（`GET /admin/api-key` / `POST /admin/api-key/rotate`）。

> 部署时建议在 `config.yaml` 中设置 `public_url`，前端 TopBar 会自动使用配置的地址。

### OpenAI 兼容客户端

Base URL 末尾带 `/v1`，API Key 填统一 API key：

```
Base URL: <public_url>/v1
API Key:  sk-bb-...        # 后台显示的统一 key，仅用于鉴权，转发时替换为账号真实 key
```

### Anthropic 兼容客户端

Base URL 末尾**不带** `/v1`，API key 填统一 API key：

```json
{
  "env": {
    "BASE_URL": "<public_url>",
    "API_KEY": "sk-bb-..."
  }
}
```

**工作原理：**
- 客户端发送 Anthropic 格式（`POST /v1/messages`）
- 代理翻译为 OpenAI 格式转发给上游，再把响应翻译回 Anthropic
- 支持流式、工具调用（Function Calling）、多轮对话、`cache_control`

> 后端对 `/v1` 前缀容错：客户端漏写（`/chat/completions`）或多写（`/v1/v1/messages`）都能按后缀正确路由。

## Dashboard 操作说明

页面顶部为**统计区**：总余额、可用账号数、窗口请求量 / 成功率 / 429 数、流量趋势时间线、各账号分担比、余额历史（可切换 1h / 24h / 7d 窗口）。下方为账号池卡片网格（支持紧凑布局）。

| 操作 | 位置 | 说明 |
|------|------|------|
| 添加账号 | 顶栏右侧 | 填入别名、API Key、AK，立即触发余额查询 |
| 删除账号 | 卡片底部 | 两次点击确认，先禁用再从池中移除并写回 config.yaml |
| 清空账号 | 顶栏右侧 | 两次点击确认，写回 config.yaml |
| 单账号测试 | 卡片底部 | 用该账号 key 直接探测上游连通性 |
| 批量测试 | 顶栏"测试全部" | 全池并发探测，逐个返回结果 |
| 查余额 | 卡片底部 | 立即查询该账号代金券余额 |
| 启用/停用 | 卡片底部 | 手动控制账号是否参与调度 |
| 解冻 | 卡片底部（冷却中时出现） | 提前解除 429 冷却 |

## 配置说明

所有配置均在项目根目录的 `config.yaml` 中管理（启动时读、运行时写回）：

```yaml
listen: ":8080"              # 后端监听端口
frontend_port: 5173          # 仅 dev.ps1 / BB_DEV 单端口模式使用
stats_db: "data/stats.db"    # 统计数据库路径，滚动保留近 7 天；Docker 需挂卷 ./data:/app/data
upstream_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"
model_override: "qwen-plus"  # 透明替换每个请求中的模型 ID（留空则不改）
public_url: "https://your-domain.com"  # 前端展示地址，留空则自动拼接为 hostname:listen端口
scheduler: "affinity"        # affinity（加权 Rendezvous，默认）| round_robin
low_balance_floor: 20.0      # 余额低于此值的账户：已映射会话迁走，但保留 floor 权重继续 drain（单位：元）
api_key: ""                  # 留空则首次启动自动生成 sk-bb-...；可在后台轮换
admin_password: "changeme"   # 明文，首次启动自动 bcrypt 加密并改写为 admin_password_hash；留空则不启用登录保护

accounts:
  - alias: "账号A"
    api_key: "sk-..."        # 模型调用密钥
    ak_id: "LTAI5..."        # AccessKey ID（用于查余额）
    ak_secret: "..."         # AccessKey Secret
```

> **端口配置**：修改 `listen` 后，需同步更新 `caddy/Caddyfile` 中的端口。建议使用 `./scripts/deploy.sh` 交互式部署脚本，它会自动从 config.yaml 读取端口并同步到各配置文件。

`ak_id` / `ak_secret` 需授予阿里云 BSS 余额查询相关权限。

## Admin API

代理端点（`/v1/*`）使用统一 API key 鉴权；管理端点（`/admin/*`）使用登录后的 Bearer token（`admin_password` 未配置时关闭登录保护）。

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/v1/chat/completions` | OpenAI 格式代理（统一 API key） |
| POST | `/v1/messages` | Anthropic 格式代理（统一 API key） |
| POST | `/admin/login` | 登录，返回 Bearer token |
| GET | `/admin/config` | 返回 `public_url` |
| GET | `/admin/accounts` | 账号池状态列表 |
| POST | `/admin/accounts` | 添加账号（JSON body），触发余额查询 |
| DELETE | `/admin/accounts` | 清空所有账号 |
| DELETE | `/admin/accounts/:id` | 删除指定账号（先禁用再移除，ID 不复用） |
| POST | `/admin/accounts/:id/toggle` | 启用/停用 |
| POST | `/admin/accounts/:id/refresh` | 立即查询余额 |
| POST | `/admin/accounts/:id/cooldown/clear` | 解除冷却 |
| POST | `/admin/accounts/:id/test` | 单账号连通性探测 |
| POST | `/admin/test-all` | 全池并发探测 |
| GET | `/admin/stats?window=1h\|24h\|7d` | 用量统计聚合（含时间线与各账号数据） |
| GET | `/admin/api-key` | 查看当前统一 API key |
| POST | `/admin/api-key/rotate` | 轮换统一 API key（即时生效） |
