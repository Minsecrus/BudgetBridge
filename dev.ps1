# Windows PowerShell development script
# Starts backend and frontend in development mode

$ErrorActionPreference = "Stop"

# Dev mode: backend acts as the single entry point, reverse-proxying the
# frontend to the vite dev server — open the backend port and you get everything.
$env:BB_DEV = "1"

# Read port configuration from config.yaml
$configPath = Join-Path $PSScriptRoot "config.yaml"
if (-not (Test-Path $configPath)) {
    Write-Error "config.yaml not found. Please copy config.yaml.example to config.yaml"
    exit 1
}

$configContent = Get-Content $configPath -Raw
$backendPort = if ($configContent -match 'listen:\s*"?:(\d+)"?') { $matches[1] } else { "8080" }
$frontendPort = if ($configContent -match 'frontend_port:\s*(\d+)') { $matches[1] } else { "5173" }

Write-Host "Starting BudgetBridge development servers..." -ForegroundColor Green
Write-Host "  入口（前端 + API）:  http://localhost:$backendPort" -ForegroundColor Cyan
Write-Host "  (dev 模式: backend 反代 vite, 只需访问上面这一个地址)" -ForegroundColor DarkGray
Write-Host ""

# Start backend
$backendCmd = "cd backend && go run main.go"
Start-Process powershell -ArgumentList "-NoExit", "-Command", $backendCmd

# Wait a bit for backend to start
Start-Sleep -Seconds 2

# Start frontend
$frontendCmd = "cd frontend && npm run dev -- --port $frontendPort"
Start-Process powershell -ArgumentList "-NoExit", "-Command", $frontendCmd

Write-Host "Development servers started. Press Ctrl+C to stop all." -ForegroundColor Green
Write-Host ""

# Keep this window open
while ($true) {
    Start-Sleep -Seconds 1
}
