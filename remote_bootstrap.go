// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/base64"
	"encoding/binary"
	"os"
	"strings"
	"unicode/utf16"
)

type startupFile struct {
	Path, Content string
	Mode          os.FileMode
}
type startupPlan struct {
	Manager string
	Files   []startupFile
	Start   [][]string
	Stop    [][]string
}

func bootstrapScript(options PairingOptions, invite, key string) string {
	if options.Platform == "windows" {
		return windowsBootstrap(options, invite, key)
	}
	args := "pair --server " + shellQuote(options.URL) + " --invite " + shellQuote(invite) + " --operator-key " + shellQuote(key)
	if options.Root {
		args += " --root"
	}
	if options.Persistent {
		args += " --persist"
	}
	if options.LogID != "" {
		args += " --log-id " + shellQuote(options.LogID) + " --log-token " + shellQuote(options.LogToken)
	}
	return `#!/bin/sh
set -eu
umask 077
case "$(uname -s)" in
  Linux) rescue_os=linux ;;
  FreeBSD) rescue_os=freebsd ;;
  Darwin) rescue_os=darwin ;;
  *) echo "No Rescue binary for this operating system" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) rescue_arch=amd64 ;;
  aarch64|arm64) rescue_arch=arm64 ;;
  i386|i486|i586|i686) rescue_arch=386 ;;
  armv6*|armv7*) rescue_arch=arm ;;
  *) echo "No Rescue binary for this architecture" >&2; exit 1 ;;
esac
rescue_dir=$(mktemp -d "${TMPDIR:-/tmp}/rescue.XXXXXXXX")
trap 'rm -rf "$rescue_dir"' EXIT HUP INT TERM
rescue_url=` + shellQuote(options.URL) + `
rescue_binary_url="$rescue_url/remote/binary/$rescue_os/$rescue_arch?invite=` + invite + `"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$rescue_binary_url" -o "$rescue_dir/rescue"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$rescue_dir/rescue" "$rescue_binary_url"
else
  fetch -q -o "$rescue_dir/rescue" "$rescue_binary_url"
fi
chmod 700 "$rescue_dir/rescue"
"$rescue_dir/rescue" ` + args + ` </dev/tty
`
}

func powershellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }

func powershellEncoded(script string) string {
	words := utf16.Encode([]rune(script))
	data := make([]byte, len(words)*2)
	for i, word := range words {
		binary.LittleEndian.PutUint16(data[i*2:], word)
	}
	return base64.StdEncoding.EncodeToString(data)
}

func windowsBootstrap(options PairingOptions, invite, key string) string {
	failureReport := ""
	if options.LogID != "" {
		failureReport = `
  $failure = $_
  try {
    $headers = @{ Authorization = ` + powershellQuote("Bearer "+options.LogToken) + ` }
    Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Method Post -Uri ` + powershellQuote(options.URL+"/api/logs/"+options.LogID+"/output") + ` -Headers $headers -Body ([Text.Encoding]::UTF8.GetBytes(($failure | Out-String))) | Out-Null
    Invoke-WebRequest -UseBasicParsing -TimeoutSec 5 -Method Post -Uri ` + powershellQuote(options.URL+"/api/logs/"+options.LogID+"/finish") + ` -Headers $headers -ContentType 'application/json' -Body '{"exitCode":1}' | Out-Null
  } catch { Write-Warning 'Rescue log upload failed' }
  throw $failure
`
	} else {
		failureReport = "\n  throw\n"
	}
	args := "pair --server " + powershellQuote(options.URL) + " --invite " + powershellQuote(invite) + " --operator-key " + powershellQuote(key)
	if options.Root {
		args += " --root"
	}
	if options.Persistent {
		args += " --persist"
	}
	if options.LogID != "" {
		args += " --log-id " + powershellQuote(options.LogID) + " --log-token " + powershellQuote(options.LogToken)
	}
	return `$ErrorActionPreference = 'Stop'
$arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
$arch = switch ($arch) { 'AMD64' { 'amd64' }; 'ARM64' { 'arm64' }; 'x86' { '386' }; default { throw "No Rescue binary for $arch" } }
$dir = Join-Path ([IO.Path]::GetTempPath()) ('rescue-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $dir | Out-Null
try {
  $binary = Join-Path $dir 'rescue.exe'
  Invoke-WebRequest -UseBasicParsing -Uri (` + powershellQuote(options.URL+"/remote/binary/windows/") + ` + $arch + ` + powershellQuote("?invite="+invite) + `) -OutFile $binary
  & $binary ` + args + `
  if ($LASTEXITCODE -ne 0) { throw "Rescue exited with code $LASTEXITCODE" }
} catch {
` + failureReport + `
} finally {
  Remove-Item -LiteralPath $dir -Recurse -Force
}
`
}
