# BudgetBridge

将多个含阿里云代金券的账号聚合为一个高可用的 OpenAI 兼容 API 服务。

## 功能

- 🔄 账号池轮询，自动故障转移
- 💰 BSS API 余额监控（每 5 分钟），低于 ¥3.00 自动停用
- ⚡ 429 限流自动冷却 60s 并换号重试
- 🌊 Stream 流式无感重试（先确认 200 再转发给客户端）
- 📊 React 看板：余额、状态、请求数、一键操作

## 快速开始

### 本地开发

```bash
# 1. 配置账号
cp backend/config.yaml.example backend/config.yaml
# 编辑 config.yaml，填入 api_key / ak_id / ak_secret

# 2. 启动后端
cd backend && go run . 

# 3. 启动前端（新终端）
cd frontend && npm install && npm run dev
# 访问 http://localhost:5173
```

### Docker 部署

```bash
cp backend/config.yaml.example backend/config.yaml
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

Key 填任意值（由后端转发时替换为真实 key）。

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

RAM 账号需要 `BSS:QueryCashCoupons` 权限。

## Admin API

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/accounts` | 账号池状态 |
| POST | `/admin/accounts/:index/toggle` | 启用/停用 |
| POST | `/admin/accounts/:index/refresh` | 立即查询余额 |
| POST | `/admin/accounts/:index/cooldown/clear` | 解除冷却 |
