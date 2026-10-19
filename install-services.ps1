# Ensure Administrator Privileges
if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Start-Process powershell.exe -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`"" -Verb RunAs
    exit
}

$mcpxDir = "D:\Andrew\Code\Github\mcpx"
$binDir = "$mcpxDir\bin"
$cfExe = "C:\Program Files (x86)\cloudflared\cloudflared.exe"
$tokenFile = "D:\Data\secrets\cloudflared-mcpx-arknights.txt"
if (-not (Test-Path $tokenFile)) { throw "缺少令牌文件：$tokenFile" }
$token = (Get-Content $tokenFile -Raw).Trim()

Write-Host "================================================================" -ForegroundColor Cyan
Write-Host "         MCPX + Cloudflare Windows Service Installer            " -ForegroundColor Cyan
Write-Host "================================================================" -ForegroundColor Cyan

# 1. Install Cloudflare Service
Write-Host ""
Write-Host "[1/2] Installing Cloudflare Tunnel Service..." -ForegroundColor Yellow
& $cfExe service install $token
Start-Service -Name "cloudflared" -ErrorAction SilentlyContinue

# 2. Install MCPX Service
Write-Host ""
Write-Host "[2/2] Installing MCPX Remote Server Service..." -ForegroundColor Yellow
Set-Location $binDir
& "$binDir\mcpx-service.exe" stop | Out-Null
& "$binDir\mcpx-service.exe" uninstall | Out-Null
& "$binDir\mcpx-service.exe" install
& "$binDir\mcpx-service.exe" start

Write-Host ""
Write-Host "================================================================" -ForegroundColor Green
Write-Host "               Installation Completed Successfully!             " -ForegroundColor Green
Write-Host "================================================================" -ForegroundColor Green
& "$binDir\mcpx-service.exe" status
Write-Host ""
Write-Host "Services are now running in background and will auto-start on boot." -ForegroundColor Cyan
Write-Host ""
Read-Host "Press Enter to exit..."
