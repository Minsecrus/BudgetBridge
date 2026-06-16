# Windows PowerShell development script
# Starts backend and frontend in development mode

$ErrorActionPreference = "Stop"

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
Write-Host "  Backend:  http://localhost:$backendPort" -ForegroundColor Cyan
Write-Host "  Frontend: http://localhost:$frontendPort" -ForegroundColor Cyan
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
