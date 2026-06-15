# 部署指南

本文档介绍如何将 BudgetBridge 部署到生产服务器。

## 目录

- [快速开始](#快速开始)
- [配置文件](#配置文件)
- [Docker 部署](#docker-部署)
- [生产环境配置](#生产环境配置)
- [客户端配置](#客户端配置)
- [维护](#维护)

## 快速开始

### 环境要求

- Docker 20.10+
- Docker Compose 2.0+

### 一键部署

```bash
git clone <repo-url>
cd BudgetBridge
cp backend/config.yaml.example backend/config.yaml
# 编辑 config.yaml
docker compose up -d --build
```

访问：
- 前端面板：http://localhost:3000
- API 端点：http://localhost:8080/v1 (OpenAI) 或 http://localhost:8080 (Anthropic)

## 配置文件

编辑 `backend/config.yaml`：

```yaml
listen: ":8080"
upstream_url: "https://your-upstream-api.com/compatible-mode/v1"
public_url: "https://your-domain.com"  # 你的域名或 IP:端口

accounts:
  - alias: "账号A"
    api_key: "sk-..."        # 模型调用密钥
    ak_id: "LTAI5..."        # AccessKey ID
    ak_secret: "..."         # AccessKey Secret
```

**字段说明：**

- `listen`: 后端监听地址，默认 `:8080`
- `upstream_url`: 上游模型服务地址（OpenAI 兼容格式）
- `public_url`: 前端展示用的公开地址，留空则自动使用请求的 hostname:listen端口
- `accounts`: 账号池配置，支持多个账号轮询

需要授予余额查询相关权限。

## Docker 部署

### 构建并启动

```bash
docker compose up -d --build
```

### 查看状态

```bash
docker compose ps
docker compose logs -f backend
```

### 停止服务

```bash
docker compose down
```

### 更新镜像

```bash
docker compose pull
docker compose up -d
```

## 生产环境配置

### Nginx 反向代理 + HTTPS

创建 `/etc/nginx/conf.d/budgetbridge.conf`：

```nginx
server {
    listen 443 ssl http2;
    server_name your-domain.com;

    ssl_certificate /etc/ssl/certs/your-cert.pem;
    ssl_certificate_key /etc/ssl/private/your-key.pem;

    # 前端
    location / {
        proxy_pass http://127.0.0.1:3000;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }

    # API
    location ~ ^/(v1|admin) {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        
        # SSE 流式响应支持
        proxy_buffering off;
        proxy_cache off;
        proxy_read_timeout 300s;
    }
}

server {
    listen 80;
    server_name your-domain.com;
    return 301 https://$host$request_uri;
}
```

测试并重载：

```bash
sudo nginx -t
sudo systemctl reload nginx
```

### 修改 Docker Compose 端口

生产环境建议只监听本地，通过 Nginx 暴露：

```yaml
services:
  backend:
    build: ./backend
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - ./backend/config.yaml:/app/config.yaml
    restart: unless-stopped

  frontend:
    build: ./frontend
    ports:
      - "127.0.0.1:3000:80"
    restart: unless-stopped
```

### 使用 systemd 管理

创建 `/etc/systemd/system/budgetbridge.service`：

```ini
[Unit]
Description=BudgetBridge API Service
After=docker.service
Requires=docker.service

[Service]
Type=notify
WorkingDirectory=/opt/BudgetBridge
ExecStart=/usr/bin/docker compose up
ExecStop=/usr/bin/docker compose down
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

启用并启动：

```bash
sudo systemctl enable budgetbridge
sudo systemctl start budgetbridge
sudo systemctl status budgetbridge
```

查看日志：

```bash
sudo journalctl -u budgetbridge -f
```

## 客户端配置

### Anthropic 兼容客户端

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "https://your-domain.com",
    "ANTHROPIC_API_KEY": "any-key"
  }
}
```

### OpenAI 兼容客户端

Base URL 设为：
```
https://your-domain.com/v1
```

API Key 填任意值（后端自动替换为真实 key）。

## 维护

### 备份配置

```bash
# 定期备份 config.yaml
cp backend/config.yaml backup/config.yaml.$(date +%Y%m%d)
```

### 查看账号状态

```bash
curl http://localhost:8080/admin/accounts | jq
```

### 添加账号

通过前端面板添加，或调用 API：

```bash
curl -X POST http://localhost:8080/admin/accounts \
  -H "Content-Type: application/json" \
  -d '{
    "alias": "账号B",
    "api_key": "sk-...",
    "ak_id": "LTAI5...",
    "ak_secret": "..."
  }'
```

### 查看日志

```bash
# Docker 日志
docker compose logs -f

# 系统日志
sudo journalctl -u budgetbridge -f
```

### 性能优化

1. **增加文件描述符限制**：
   ```bash
   ulimit -n 65535
   ```

2. **调整 Docker 资源限制**：
   ```yaml
   services:
     backend:
       deploy:
         resources:
           limits:
             cpus: '2.0'
             memory: 1G
   ```

3. **启用 Gzip 压缩**（Nginx）：
   ```nginx
   gzip on;
   gzip_types application/json text/event-stream;
   ```

## 故障排查

### 账号余额查询失败

检查权限和 AccessKey 是否正确：

```bash
docker compose exec backend ./budgetbridge -check-account 0
```

### 429 限流

查看冷却中的账号：

```bash
curl http://localhost:8080/admin/accounts | jq '.[] | select(.cooldown_secs > 0)'
```

手动解除冷却：

```bash
curl -X POST http://localhost:8080/admin/accounts/0/cooldown/clear
```

### 连接超时

检查 Nginx 配置中的 `proxy_read_timeout` 是否足够长（建议 300s）。
