<#
.SYNOPSIS
  Install bwagent on Windows as a system service.

.DESCRIPTION
  Downloads the bwagent binary from GitHub Releases, writes the configuration
  file, registers a Windows service that starts automatically at boot, and
  starts the service.

  Run this script as Administrator in PowerShell:
    powershell -NoProfile -ExecutionPolicy Bypass -Command `
      "& ([scriptblock]::Create((Invoke-WebRequest -UseBasicParsing 'https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.ps1').Content)) `
        -ServerUrl 'http://1.2.3.4:8080' -InitToken 'YourToken' -ClientName 'my-vps'"

.PARAMETER ServerUrl
  The URL of the bwtest panel (required). Example: http://1.2.3.4:8080
.PARAMETER InitToken
  The init token from the panel (required).
.PARAMETER ClientName
  A descriptive name for this client. Defaults to the computer name.
.PARAMETER Remark
  Optional remark / note.
.PARAMETER Version
  Release version to download. Defaults to "latest".
#>
param(
  [Parameter(Mandatory = $true)]  [string]$ServerUrl,
  [Parameter(Mandatory = $true)]  [string]$InitToken,
  [string]$ClientName = $env:COMPUTERNAME,
  [string]$Remark     = '',
  [string]$Version    = 'latest'
)

$ErrorActionPreference = 'Stop'

$ServiceName = 'bwagent'
$DisplayName = 'Bandwidth Test Agent'
$InstallDir  = "$env:ProgramFiles\bwagent"
$BinPath     = "$InstallDir\bwagent.exe"
$ConfigDir   = "$env:ProgramData\bwagent"
$ConfigFile  = "$ConfigDir\config.json"
$Repo        = 'ctsunny/bwtest'

function Log-Info  { param([string]$msg) Write-Host "[INFO] $msg" -ForegroundColor Green  }
function Log-Error { param([string]$msg) Write-Host "[ERR ] $msg" -ForegroundColor Red    }

# ── Require Administrator ──────────────────────────────────────────────────────
$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
  Log-Error 'Please run this script as Administrator.'
  exit 1
}

# ── Create directories ─────────────────────────────────────────────────────────
New-Item -ItemType Directory -Force -Path $InstallDir  | Out-Null
New-Item -ItemType Directory -Force -Path $ConfigDir   | Out-Null

# ── Download binary ────────────────────────────────────────────────────────────
if ($Version -eq 'latest') {
  $DlUrl = "https://github.com/$Repo/releases/latest/download/bwagent-windows-amd64.exe"
} else {
  $DlUrl = "https://github.com/$Repo/releases/download/$Version/bwagent-windows-amd64.exe"
}

Log-Info "Downloading bwagent from $DlUrl ..."
try {
  # Stop the service first so the executable can be overwritten
  if (Get-Service -Name $ServiceName -ErrorAction SilentlyContinue) {
    Log-Info "Stopping existing $ServiceName service..."
    Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
    Start-Sleep -Seconds 2
  }
  Invoke-WebRequest -Uri $DlUrl -OutFile $BinPath -UseBasicParsing
} catch {
  Log-Error "Download failed: $_"
  exit 1
}
Log-Info "Binary saved to $BinPath"

# ── Write config ───────────────────────────────────────────────────────────────
if (Test-Path $ConfigFile) {
  Log-Info "Updating existing config: server_url / init_token / name"
  $cfg = Get-Content $ConfigFile -Raw | ConvertFrom-Json
  $cfg.server_url  = $ServerUrl
  $cfg.init_token  = $InitToken
  $cfg.name        = $ClientName
  if ($Remark -ne '') { $cfg | Add-Member -NotePropertyName remark -NotePropertyValue $Remark -Force }
  # Use UTF-8 without BOM; PowerShell 5's -Encoding UTF8 adds a BOM which breaks Go's JSON parser.
  [System.IO.File]::WriteAllText($ConfigFile, ($cfg | ConvertTo-Json), (New-Object System.Text.UTF8Encoding $false))
} else {
  $cfg = [ordered]@{
    server_url    = $ServerUrl
    name          = $ClientName
    init_token    = $InitToken
    client_id     = ''
    client_token  = ''
  }
  if ($Remark -ne '') { $cfg['remark'] = $Remark }
  # Use UTF-8 without BOM; PowerShell 5's -Encoding UTF8 adds a BOM which breaks Go's JSON parser.
  [System.IO.File]::WriteAllText($ConfigFile, ($cfg | ConvertTo-Json), (New-Object System.Text.UTF8Encoding $false))
}
Log-Info "Config written to $ConfigFile"

# ── Register Windows service ───────────────────────────────────────────────────
$existingSvc = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
if ($existingSvc) {
  Log-Info "Removing existing service..."
  Stop-Service -Name $ServiceName -Force -ErrorAction SilentlyContinue
  Start-Sleep -Seconds 2
  & sc.exe delete $ServiceName | Out-Null
  Start-Sleep -Seconds 2
}

Log-Info "Registering service '$ServiceName'..."
New-Service -Name $ServiceName `
            -DisplayName $DisplayName `
            -BinaryPathName "`"$BinPath`" `"$ConfigFile`"" `
            -StartupType Automatic `
            -Description "Automatically runs the bwtest bandwidth-test agent."
# Restart on failure: after 60 s reset, restart 3 times with 3 s delay between attempts
& sc.exe failure $ServiceName reset= 60 actions= restart/3000/restart/3000/restart/3000 | Out-Null

Log-Info "Starting service..."
Start-Service -Name $ServiceName

Log-Info "Installation complete."
Write-Host ""
Write-Host "Config file : $ConfigFile"
Write-Host "Useful commands:"
Write-Host "  Get-Service $ServiceName"
Write-Host "  Stop-Service $ServiceName"
Write-Host "  Start-Service $ServiceName"
Write-Host "  Get-EventLog -LogName Application -Source $ServiceName -Newest 20"
