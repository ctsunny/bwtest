# bwtest

A lightweight bandwidth test panel and agent for Linux and Windows.

## Features

- Server-side web panel (HTTP Basic Auth)
- Client approval workflow
- Upload / download / traditional / browse / stream / backup task modes
- Configurable speed (Mbps) and duration (seconds)
- SQLite persistence (no external DB required)
- SSE live refresh in the panel
- Client reports SSH failed-login attempts from the last hour (Linux)
- Bark notifications for new clients, tasks, and successful SSH logins
- Linux amd64 / arm64 and Windows amd64 release binaries via GitHub Actions
- Windows client runs as a system service (auto-start on boot)

## Install server

```bash
curl --proto '=https' --tlsv1.2 -fsSL \
  https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_server.sh | \
sudo bash -s -- \
  --server-host 1.2.3.4 \
  --panel-port 8080 \
  --data-port 9000 \
  --admin-user admin \
  --admin-pass 'YourStrongPass123' \
  --init-token 'YourInitToken123'
```

Installation output example:

```
Panel URL : http://1.2.3.4:8080/admin
Admin User: admin
Admin Pass: <generated or provided>
Init Token: <generated or provided>
```

## Install client

### Linux

```bash
curl --proto '=https' --tlsv1.2 -fsSL \
  https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.sh | \
sudo bash -s -- \
  --server-url http://1.2.3.4:8080 \
  --init-token 'YourInitToken123' \
  --client-name "$(hostname)"
```

### Windows (run in PowerShell **as Administrator**)

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -Command "& ([scriptblock]::Create((Invoke-WebRequest -UseBasicParsing 'https://raw.githubusercontent.com/ctsunny/bwtest/main/scripts/install_client.ps1').Content)) -ServerUrl 'http://1.2.3.4:8080' -InitToken 'YourInitToken123' -ClientName 'my-vps'"
```

> **Tip:** The web panel's **接入新客户端** modal can generate the exact command for you —
> choose *Linux (bash)* or *Windows (PowerShell)* from the **平台** dropdown.

## Usage

1. Open `http://1.2.3.4:8080/admin` and log in.
2. Wait for the client to appear in the Clients table.
3. Click **Approve** to allow the client to receive tasks.
4. Fill in the **Create Task** form:
   - `client_id`: copy from the Clients table
   - `mode`: `upload` / `download` / `traditional` / `browse` / `stream` / `backup`
   - `up_mbps` / `down_mbps`: speed limit in Mbps
   - `duration_sec`: how many seconds to run
5. Click **Create Task**. The panel auto-refreshes via SSE.
6. To stop a running task early, click **Stop** in the Tasks table.

### Troubleshooting

- If a task appears as `pending` for a long time after service restarts, use the panel's **取消/停止** action to mark it `stopped` and recreate the task.
- If action buttons in the web panel do not respond, hard refresh the browser (Ctrl+F5) and verify Basic Auth is still valid.

## Useful commands

### Linux

```bash
# Server
systemctl status bwpanel
journalctl -u bwpanel -f
cat /etc/default/bwpanel

# Client
systemctl status bwagent
journalctl -u bwagent -f
cat /etc/bwagent/config.json
```

### Windows (PowerShell)

```powershell
# Client
Get-Service bwagent
Stop-Service bwagent
Start-Service bwagent
Get-EventLog -LogName Application -Source bwagent -Newest 20
Get-Content "$env:ProgramData\bwagent\config.json"
```

## Uninstall completely

### Uninstall Server (Linux)

```bash
systemctl stop bwpanel
systemctl disable bwpanel
rm -f /etc/systemd/system/bwpanel.service
rm -f /usr/local/bin/bwpanel /usr/local/bin/bwpanel-menu
rm -f /etc/default/bwpanel
rm -rf /opt/bwtest
systemctl daemon-reload
```

### Uninstall Client — Linux

```bash
systemctl stop bwagent
systemctl disable bwagent
rm -f /etc/systemd/system/bwagent.service
rm -f /usr/local/bin/bwagent
rm -rf /etc/bwagent
rm -rf /opt/bwtest/client_source
systemctl daemon-reload
```

### Uninstall Client — Windows (PowerShell **as Administrator**)

```powershell
Stop-Service bwagent -Force -ErrorAction SilentlyContinue
sc.exe delete bwagent
Remove-Item -Recurse -Force "$env:ProgramFiles\bwagent"
Remove-Item -Recurse -Force "$env:ProgramData\bwagent"
```

## Release a new version

```bash
git tag v0.1.0
git push origin v0.1.0
```

GitHub Actions will build binaries for linux/amd64, linux/arm64, and windows/amd64 and attach them to the release automatically.
