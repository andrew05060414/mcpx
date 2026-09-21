if (-not ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Start-Process powershell.exe -ArgumentList "-NoProfile -ExecutionPolicy Bypass -File `"$PSCommandPath`"" -Verb RunAs
    exit
}

$binDir = "D:\Andrew\Code\Github\mcpx\bin"
Write-Host "Stopping and removing services..." -ForegroundColor Yellow
& "$binDir\mcpx-service.exe" stop | Out-Null
& "$binDir\mcpx-service.exe" uninstall | Out-Null
Stop-Service -Name "cloudflared" -ErrorAction SilentlyContinue
& "C:\Program Files (x86)\cloudflared\cloudflared.exe" service uninstall | Out-Null
Write-Host "Services uninstalled." -ForegroundColor Green
Read-Host "Press Enter to exit..."
