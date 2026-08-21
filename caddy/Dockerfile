# 多阶段构建：先构建前端静态产物，再用 Caddy 运行时托管
# 单一入口架构 —— Caddy 同时承担反向代理（→ backend）与前端静态托管（/srv）
FROM node:20-alpine AS builder
WORKDIR /app
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci
COPY frontend/ .
RUN npm run build

FROM caddy:2-alpine
# 前端构建产物，由 Caddyfile 中的 handle { file_server } 托管
COPY --from=builder /app/dist /srv
# Caddyfile 不内置进镜像：运行时由 docker-compose 以 volume 挂载
# （./caddy/Caddyfile:/etc/caddy/Caddyfile），deploy.sh 会在 up 前 sed 替换占位符
