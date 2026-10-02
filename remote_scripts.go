// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
)

func (a *App) handleRunScript(w http.ResponseWriter, r *http.Request) {
	script, ok := a.store.ScriptByFilename(r.PathValue("name"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	platform := r.URL.Query().Get("platform")
	if platform != "" && platform != "unix" && platform != "windows" {
		http.Error(w, "invalid script platform", 400)
		return
	}
	id, token, err := a.logs.Create(script.Filename, r.RemoteAddr)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if platform == "windows" {
		fmt.Fprint(w, windowsRunScript(script.Content, base, id, token))
		return
	}
	fmt.Fprint(w, unixRunScript(script.Content, base, id, token))
}

func unixRunScript(content, base, id, token string) string {
	delimiter := "RESCUE_" + strings.ToUpper(id)
	return `#!/usr/bin/env bash
rescue_dir=$(mktemp -d "${TMPDIR:-/tmp}/rescue-script.XXXXXXXX") || exit 1
trap 'rm -rf "$rescue_dir"' EXIT
cat >"$rescue_dir/script.sh" <<'` + delimiter + `'
` + content + `
` + delimiter + `
if ( : </dev/tty ) 2>/dev/null; then
  bash "$rescue_dir/script.sh" </dev/tty 2>&1 | tee "$rescue_dir/output.log"
  rescue_code=${PIPESTATUS[0]}
else
  bash "$rescue_dir/script.sh" </dev/null 2>&1 | tee "$rescue_dir/output.log"
  rescue_code=${PIPESTATUS[0]}
fi
curl -fsS -X POST -H ` + shellQuote("Authorization: Bearer "+token) + ` --data-binary "@$rescue_dir/output.log" ` + shellQuote(base+"/api/logs/"+id+"/output") + ` >/dev/null || echo 'Rescue log upload failed' >&2
curl -fsS -X POST -H ` + shellQuote("Authorization: Bearer "+token) + ` -H 'Content-Type: application/json' --data "{\"exitCode\":$rescue_code}" ` + shellQuote(base+"/api/logs/"+id+"/finish") + ` >/dev/null || echo 'Rescue log status upload failed' >&2
exit "$rescue_code"
`
}

func windowsRunScript(content, base, id, token string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	return `$ErrorActionPreference = 'Stop'
$dir = Join-Path ([IO.Path]::GetTempPath()) ('rescue-script-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir | Out-Null
$scriptPath = Join-Path $dir 'script.ps1'
$logPath = Join-Path $dir 'output.log'
$output = New-Object IO.StreamWriter($logPath, $false, (New-Object Text.UTF8Encoding($false)))
$code = 0
$uploaded = $false
try {
  $script = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String(` + powershellQuote(encoded) + `))
  [IO.File]::WriteAllText($scriptPath, $script, (New-Object Text.UTF8Encoding($true)))
  $shell = (Get-Process -Id $PID).Path
  $ErrorActionPreference = 'Continue'
  & $shell -NoLogo -NoProfile -ExecutionPolicy Bypass -File $scriptPath 2>&1 | ForEach-Object { $text = ($_ | Out-String); $output.Write($text); Write-Host $text -NoNewline }
  $code = $LASTEXITCODE
  $ErrorActionPreference = 'Stop'
} catch {
  $text = ($_ | Out-String)
  $output.Write($text)
  Write-Host $text -NoNewline
  $code = 1
} finally {
  $output.Dispose()
  $headers = @{ Authorization = ` + powershellQuote("Bearer "+token) + ` }
  try {
    Invoke-WebRequest -UseBasicParsing -Method Post -Uri ` + powershellQuote(base+"/api/logs/"+id+"/output") + ` -Headers $headers -InFile $logPath -ContentType 'text/plain; charset=utf-8' | Out-Null
    Invoke-WebRequest -UseBasicParsing -Method Post -Uri ` + powershellQuote(base+"/api/logs/"+id+"/finish") + ` -Headers $headers -ContentType 'application/json' -Body ('{"exitCode":' + $code + '}') | Out-Null
    $uploaded = $true
  } catch { Write-Warning ('Rescue log upload failed; output saved in ' + $logPath) }
  if ($uploaded) { Remove-Item -LiteralPath $dir -Recurse -Force }
}
if ($code -ne 0) { throw "Script exited with code $code" }
`
}
