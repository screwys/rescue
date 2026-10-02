// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	installedDir      = filepath.Join(os.Getenv("ProgramData"), "RescueAgent")
	installedBinary   = filepath.Join(installedDir, "rescue.exe")
	installedConfig   = filepath.Join(installedDir, "agent.json")
	installedManifest = filepath.Join(installedDir, "startup.json")
)

func exists(path string) bool                 { _, err := os.Stat(path); return err == nil }
func startupManager() (string, string, error) { return "windows", "", nil }
func checkStartupSupport() error {
	return nil
}

func windowsTaskScript() string {
	return `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$cfg = Get-Content -LiteralPath ` + powershellQuote(installedConfig) + ` -Raw -Encoding UTF8 | ConvertFrom-Json
$arguments = ` + powershellQuote(`agent --config "`+installedConfig+`" --server "`) + ` + $cfg.url + '"'
$action = New-ScheduledTaskAction -Execute ` + powershellQuote(installedBinary) + ` -Argument $arguments
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 255 -RestartInterval (New-TimeSpan -Minutes 1) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
if ($cfg.root) {
  $principal = New-ScheduledTaskPrincipal -UserId 'S-1-5-18' -LogonType ServiceAccount -RunLevel Highest
  $task = New-ScheduledTask -Action $action -Trigger $trigger -Settings $settings -Principal $principal
  Register-ScheduledTask -TaskName 'RescueAgent' -InputObject $task -Force | Out-Null
} else {
  $secure = Read-Host -Prompt ('Password for ' + $cfg.user) -AsSecureString
  $credential = New-Object System.Management.Automation.PSCredential($cfg.user, $secure)
  $principal = New-ScheduledTaskPrincipal -UserId $cfg.sid -LogonType Password -RunLevel Limited
  $task = New-ScheduledTask -Action $action -Trigger $trigger -Settings $settings -Principal $principal
  Register-ScheduledTask -TaskName 'RescueAgent' -InputObject $task -User $cfg.user -Password $credential.GetNetworkCredential().Password -Force | Out-Null
  $credential = $null
  $secure.Dispose()
}
Enable-ScheduledTask -TaskName 'RescueAgent' | Out-Null
Start-ScheduledTask -TaskName 'RescueAgent'
`
}

func makeStartupPlan(manager, directory string) startupPlan {
	if manager != "windows" {
		return startupPlan{}
	}
	return startupPlan{Manager: manager, Files: []startupFile{{Path: filepath.Join(installedDir, "startup.ps1"), Content: windowsTaskScript(), Mode: 0o600}}}
}

func setInstalledACL(path, sid string) error {
	access := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid != "S-1-5-18" {
		access += "(A;;FRFX;;;" + sid + ")"
	}
	sd, err := windows.SecurityDescriptorFromString(access)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	owner, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
}

func installAgent(sourceConfig string) error {
	if !isAdministrator() {
		return errors.New("startup installation requires administrator access")
	}
	cfg, err := loadAgentConfig(sourceConfig)
	if err != nil {
		return err
	}
	stop := `$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$task = Get-ScheduledTask -TaskPath '\' | Where-Object { $_.TaskName -eq 'RescueAgent' }
if ($task) {
  Disable-ScheduledTask -TaskName 'RescueAgent' | Out-Null
  if ((Get-ScheduledTask -TaskName 'RescueAgent').State -eq 'Running') {
    Stop-ScheduledTask -TaskName 'RescueAgent'
  }
  for ($i = 0; $i -lt 100; $i++) {
    if ((Get-ScheduledTask -TaskName 'RescueAgent').State -ne 'Running') { break }
    Start-Sleep -Milliseconds 100
  }
  if ((Get-ScheduledTask -TaskName 'RescueAgent').State -eq 'Running') { throw 'Rescue task did not stop' }
}`
	stopCmd := exec.Command(cfg.Shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand", powershellEncoded(stop))
	stopCmd.Stdout, stopCmd.Stderr = commandStdout, commandStderr
	if err := stopCmd.Run(); err != nil {
		return fmt.Errorf("stop previous Rescue task: %w", err)
	}
	if err := os.MkdirAll(installedDir, 0o700); err != nil {
		return err
	}
	if err := setInstalledACL(installedDir, cfg.SID); err != nil {
		return err
	}
	if err := writePrivateJSON(installedConfig, cfg); err != nil {
		return err
	}
	if err := setInstalledACL(installedConfig, cfg.SID); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	if binary != installedBinary {
		in, err := os.Open(binary)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(installedBinary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	if err := setInstalledACL(installedBinary, cfg.SID); err != nil {
		return err
	}
	cmd := exec.Command(cfg.Shell, "-NoLogo", "-NoProfile", "-OutputFormat", "Text", "-EncodedCommand", powershellEncoded(windowsTaskScript()))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, commandStdout, commandStderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("startup installation: %w; credentials retained at %s", err, installedConfig)
	}
	fmt.Fprintf(commandStdout, "Paired %s; starts at boot\n", cfg.Name)
	return nil
}

func uninstallAgent() error {
	if !isAdministrator() {
		return elevate([]string{"uninstall"})
	}
	if !exists(installedConfig) {
		return errors.New("no installed Rescue agent")
	}
	script := "$ErrorActionPreference='Stop'; Stop-ScheduledTask -TaskName 'RescueAgent'; Unregister-ScheduledTask -TaskName 'RescueAgent' -Confirm:$false"
	cmd := exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", powershellEncoded(script))
	cmd.Stdout, cmd.Stderr = commandStdout, commandStderr
	if err := cmd.Run(); err != nil {
		return err
	}
	// Windows cannot delete the executable while this process is running.
	cleanup := "Wait-Process -Id " + fmt.Sprint(os.Getpid()) + " -ErrorAction SilentlyContinue; for ($i=0; $i -lt 100; $i++) { try { Remove-Item -LiteralPath " + powershellQuote(installedDir) + " -Recurse -Force -ErrorAction Stop; break } catch { Start-Sleep -Milliseconds 100 } }"
	cmd = exec.Command("powershell.exe", "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", powershellEncoded(cleanup))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Println("Rescue startup removed; revoke its pairing on the operator computer")
	return nil
}
