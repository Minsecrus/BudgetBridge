# BudgetBridge

将多个含阿里云代金券的账号聚合为一个高可用的 OpenAI 兼容 API 服务。

## 功能

- 🔄 账号池轮询，自动故障转移
- 💰 BSS API 余额监控（每 5 分钟），低于 ¥3.00 自动停用
- ⚡ 429 限流自动冷却 60s 并换号重试
- 🌊 Stream 流式无感重试（先确认 200 再转发给客户端）
- 📊 React 看板：账号增删、余额查询、启停、测试、清空

## 快速开始

### 本地开发（推荐）

```powershell
# 1. 配置账号
copy backend\config.yaml.example backend\config.yaml
# 编辑 config.yaml，填入真实的 api_key / ak_id / ak_secret

# 2. 一键启动（弹出两个终端窗口）
.\dev.ps1
# 前端: http://localhost:5173
# 后端: http://localhost:8080
```

> 需要先安装 [air](https://github.com/air-verse/air) 实现后端热重载：
> `go install github.com/air-verse/air@latest`
> 未安装时 `dev.ps1` 自动回退到 `go run .`

### Docker 部署

```bash
copy backend\config.yaml.example backend\config.yaml
# 编辑 config.yaml

docker compose up -d
# 前端: http://localhost:3000
# API:  http://localhost:8080/v1
```

## API 接入

在 New API 或其他客户端中，将 Base URL 设为：

```
http://your-host:8080/v1
```

Key 填任意值（后端转发时自动替换为真实 key）。

## Dashboard 操作说明

| 操作 | 位置 | 说明 |
|------|------|------|
| 添加账号 | 顶栏右侧 | 填入别名、API Key、AK，立即触发余额查询 |
| 清空账号 | 顶栏右侧 | 两次点击确认，写回 config.yaml |
| 端到端测试 | 顶栏"测试"按钮 | 直接向代理发送真实请求，无需配置外部工具 |
| 查余额 | 卡片底部 | 立即向阿里云 BSS 查询该账号代金券余额 |
| 启用/停用 | 卡片底部 | 手动控制账号是否参与轮询 |
| 解冻 | 卡片底部（冷却中时出现） | 提前解除 429 冷却 |

## 配置说明

```yaml
listen: ":8080"
upstream_url: "https://dashscope.aliyuncs.com/compatible-mode/v1"

accounts:
  - alias: "账号A"
    api_key: "sk-..."        # 模型调用密钥
    ak_id: "LTAI5..."        # 阿里云 RAM AccessKey ID
    ak_secret: "..."         # 阿里云 RAM AccessKey Secret
```

RAM 账号需授予 `AliyunBSSReadOnlyAccess` 策略（含 `bss:QueryCashCoupons` 权限）。

## Admin API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/accounts` | 账号池状态列表 |
| POST | `/admin/accounts` | 添加账号（JSON body） |
| DELETE | `/admin/accounts` | 清空所有账号 |
| POST | `/admin/accounts/:index/toggle` | 启用/停用 |
| POST | `/admin/accounts/:index/refresh` | 立即查询余额 |
| POST | `/admin/accounts/:index/cooldown/clear` | 解除冷却 |
