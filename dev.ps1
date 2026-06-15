# dev.ps1 — start backend + frontend in separate windows
$root = $PSScriptRoot

# backend: prefer air, fallback to go run
$backendCmd = if (Get-Command air -ErrorAction SilentlyContinue) { "air" } else { "go run ." }

Start-Process powershell -ArgumentList "-NoExit", "-Command", "cd '$root\backend'; $backendCmd"
Start-Process powershell -ArgumentList "-NoExit", "-Command", "cd '$root\frontend'; npm run dev"

Write-Host "Started:" -ForegroundColor Green
Write-Host "  Backend  http://localhost:8080"
Write-Host "  Frontend http://localhost:5173"
