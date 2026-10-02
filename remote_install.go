//go:build !windows

// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	installedDir      = "/usr/local/lib/rescue-agent"
	installedBinary   = installedDir + "/rescue"
	installedConfig   = installedDir + "/agent.json"
	installedManifest = installedDir + "/startup.json"
)

func elevate(args []string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if sudo, err := exec.LookPath("sudo"); err == nil {
		cmd = exec.Command(sudo, append([]string{"--", binary}, args...)...)
	} else if doas, err := exec.LookPath("doas"); err == nil {
		cmd = exec.Command(doas, append([]string{binary}, args...)...)
	} else {
		words := []string{shellQuote(binary)}
		for _, arg := range args {
			words = append(words, shellQuote(arg))
		}
		cmd = exec.Command("su", "root", "-c", strings.Join(words, " "))
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("administrator access needs a terminal: %w", err)
	}
	defer tty.Close()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, commandStdout, commandStderr
	return cmd.Run()
}

func exists(path string) bool    { _, err := os.Stat(path); return err == nil }
func available(name string) bool { _, err := exec.LookPath(name); return err == nil }

func startupManager() (string, string, error) {
	if runtime.GOOS == "freebsd" {
		return "freebsd", "", nil
	}
	if runtime.GOOS == "darwin" {
		return "launchd", "", nil
	}
	if runtime.GOOS != "linux" {
		return "", "", errors.New("startup installation requires a Unix system")
	}
	if exists("/run/systemd/system") && available("systemctl") {
		return "systemd", "", nil
	}
	if available("rc-service") && available("rc-update") && (exists("/run/openrc") || exists("/lib/rc/init.d") || exists("/run/rc")) {
		return "openrc", "", nil
	}
	if available("dinitctl") && (exists("/run/dinitctl") || exists("/run/dinit.d") || processRunning("dinit")) {
		return "dinit", "", nil
	}
	if available("sv") {
		if dir := supervisorDirectory("runsvdir"); dir != "" {
			return "runit", dir, nil
		}
	}
	if available("s6") && processRunning("s6-svscan") {
		if _, err := exec.Command("s6", "repository", "help").Output(); err == nil {
			return "s6", s6SourceDirectory(), nil
		}
	}
	if available("update-rc.d") && exists("/etc/init.d") {
		return "sysv", "", nil
	}
	if available("chkconfig") && exists("/etc/init.d") {
		return "sysv-chkconfig", "", nil
	}
	return "", "", errors.New("no supported startup manager found; temporary pairing still works")
}

func s6SourceDirectory() string {
	stores := "/usr/share/s6-frontend/s6-rc/sources:/etc/s6-frontend/s6-rc/sources"
	data, _ := os.ReadFile("/etc/s6-frontend.conf")
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "storelist" {
			value = strings.Trim(strings.TrimSpace(value), "\"'")
			if value != "" {
				stores = value
			}
		}
	}
	directories := strings.Split(stores, ":")
	return directories[len(directories)-1]
}

func processRunning(name string) bool {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err == nil && strings.TrimSpace(string(comm)) == name {
			return true
		}
	}
	return false
}

func supervisorDirectory(name string) string {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		comm, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(comm)) != name {
			continue
		}
		status, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		root := false
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") {
				values := strings.Fields(line)
				root = len(values) > 1 && values[1] == "0"
			}
		}
		if !root {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		args := strings.Split(string(cmdline), "\x00")
		for _, arg := range args[1:] {
			if arg == "" || strings.HasPrefix(arg, "-") {
				continue
			}
			if !filepath.IsAbs(arg) {
				cwd, _ := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
				arg = filepath.Join(cwd, arg)
			}
			if real, err := filepath.EvalSymlinks(arg); err == nil {
				return real
			}
			break
		}
	}
	return ""
}

func checkStartupSupport() error {
	if exists(installedDir) {
		return errors.New("Rescue is already installed; run rescue uninstall before pairing it again")
	}
	_, _, err := startupManager()
	return err
}

func makeStartupPlan(manager, directory string) startupPlan {
	command := installedBinary + " agent --config " + installedConfig
	plan := startupPlan{Manager: manager}
	file := func(path, content string, mode os.FileMode) {
		plan.Files = append(plan.Files, startupFile{path, content, mode})
	}
	switch manager {
	case "systemd":
		file("/etc/systemd/system/rescue-agent.service", "[Unit]\nDescription=Rescue remote access\nAfter=network.target\n\n[Service]\nExecStart="+command+"\nRestart=on-failure\nRestartSec=3\n\n[Install]\nWantedBy=multi-user.target\n", 0o644)
		plan.Start = [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", "--now", "rescue-agent.service"}}
		plan.Stop = [][]string{{"systemctl", "disable", "--now", "rescue-agent.service"}}
	case "openrc":
		runner, _ := exec.LookPath("openrc-run")
		if runner == "" {
			runner = "/sbin/openrc-run"
		}
		file("/etc/init.d/rescue-agent", "#!"+runner+"\nname=\"Rescue remote access\"\ncommand="+shellQuote(installedBinary)+"\ncommand_args="+shellQuote("agent --config "+installedConfig)+"\ncommand_background=true\npidfile=/run/rescue-agent.pid\ndepend() { need net; }\n", 0o755)
		plan.Start = [][]string{{"rc-update", "add", "rescue-agent", "default"}, {"rc-service", "rescue-agent", "start"}}
		plan.Stop = [][]string{{"rc-service", "rescue-agent", "stop"}, {"rc-update", "del", "rescue-agent", "default"}}
	case "runit":
		// Install in the actual persistent scan directory, including custom layouts.
		file(filepath.Join(directory, "rescue-agent", "run"), "#!/bin/sh\nexec "+command+" 2>&1\n", 0o755)
		plan.Start = [][]string{{"sv", "-w", "10", "up", filepath.Join(directory, "rescue-agent")}}
		plan.Stop = [][]string{{"sv", "down", filepath.Join(directory, "rescue-agent")}}
	case "dinit":
		file("/etc/dinit.d/rescue-agent", "type = process\ncommand = "+command+"\nrestart = true\nrestart-delay = 3\nlog-type = buffer\n", 0o644)
		plan.Start = [][]string{{"dinitctl", "enable", "rescue-agent"}, {"dinitctl", "start", "rescue-agent"}}
		plan.Stop = [][]string{{"dinitctl", "stop", "rescue-agent"}, {"dinitctl", "disable", "rescue-agent"}}
	case "s6":
		file(filepath.Join(directory, "rescue-agent", "type"), "longrun\n", 0o644)
		file(filepath.Join(directory, "rescue-agent", "run"), "#!/bin/sh\nexec "+command+" >>/var/log/rescue-agent.log 2>&1\n", 0o755)
		plan.Start = [][]string{{"s6", "repository", "sync"}, {"s6", "set", "enable", "rescue-agent"}, {"s6", "set", "commit"}, {"s6", "live", "install"}, {"s6", "live", "start", "rescue-agent"}}
		plan.Stop = [][]string{{"s6", "live", "stop", "rescue-agent"}, {"s6", "set", "disable", "rescue-agent"}, {"s6", "set", "commit"}, {"s6", "live", "install"}}
	case "freebsd":
		file("/usr/local/etc/rc.d/rescue_agent", `#!/bin/sh
# PROVIDE: rescue_agent
# REQUIRE: NETWORKING
# KEYWORD: shutdown
. /etc/rc.subr
name="rescue_agent"
rcvar="rescue_agent_enable"
pidfile="/var/run/rescue_agent.pid"
command="/usr/sbin/daemon"
command_args="-r -P $pidfile `+command+`"
load_rc_config "$name"
: ${rescue_agent_enable:=NO}
run_rc_command "$1"
`, 0o755)
		plan.Start = [][]string{{"sysrc", "rescue_agent_enable=YES"}, {"service", "rescue_agent", "start"}}
		plan.Stop = [][]string{{"service", "rescue_agent", "stop"}, {"sysrc", "-x", "rescue_agent_enable"}}
	case "launchd":
		file("/Library/LaunchDaemons/org.rescue.agent.plist", `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>Label</key><string>org.rescue.agent</string><key>ProgramArguments</key><array><string>`+installedBinary+`</string><string>agent</string><string>--config</string><string>`+installedConfig+`</string></array><key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>StandardErrorPath</key><string>/var/log/rescue-agent.log</string></dict></plist>
`, 0o644)
		plan.Start = [][]string{{"launchctl", "bootstrap", "system", "/Library/LaunchDaemons/org.rescue.agent.plist"}}
		plan.Stop = [][]string{{"launchctl", "bootout", "system/org.rescue.agent"}}
	case "sysv", "sysv-chkconfig":
		file("/etc/init.d/rescue-agent", `#!/bin/sh
### BEGIN INIT INFO
# Provides: rescue-agent
# Required-Start: $network
# Required-Stop: $network
# Default-Start: 2 3 4 5
# Default-Stop: 0 1 6
### END INIT INFO
# chkconfig: 2345 99 01
# description: Rescue remote access
case "$1" in
start)
  if test -f /run/rescue-agent.pid && kill -0 "$(cat /run/rescue-agent.pid)" 2>/dev/null; then exit 0; fi
  `+command+` </dev/null >>/var/log/rescue-agent.log 2>&1 &
  echo $! >/run/rescue-agent.pid ;;
stop)
  if test -f /run/rescue-agent.pid; then kill "$(cat /run/rescue-agent.pid)" 2>/dev/null || :; rm -f /run/rescue-agent.pid; fi ;;
restart) "$0" stop; "$0" start ;;
*) exit 1 ;;
esac
`, 0o755)
		if manager == "sysv" {
			plan.Start = [][]string{{"update-rc.d", "rescue-agent", "defaults"}, {"/etc/init.d/rescue-agent", "start"}}
			plan.Stop = [][]string{{"/etc/init.d/rescue-agent", "stop"}, {"update-rc.d", "-f", "rescue-agent", "remove"}}
		} else {
			plan.Start = [][]string{{"chkconfig", "--add", "rescue-agent"}, {"chkconfig", "rescue-agent", "on"}, {"/etc/init.d/rescue-agent", "start"}}
			plan.Stop = [][]string{{"/etc/init.d/rescue-agent", "stop"}, {"chkconfig", "--del", "rescue-agent"}}
		}
	}
	return plan
}

func runStartupCommands(commands [][]string) error {
	for _, words := range commands {
		cmd := exec.Command(words[0], words[1:]...)
		cmd.Stdout, cmd.Stderr = commandStdout, commandStderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", strings.Join(words, " "), err)
		}
	}
	return nil
}

func installAgent(sourceConfig string) error {
	if os.Geteuid() != 0 {
		return errors.New("startup installation requires administrator access")
	}
	cfg, err := loadAgentConfig(sourceConfig)
	if err != nil {
		return err
	}
	manager, directory, err := startupManager()
	if err != nil {
		return err
	}
	if exists(installedConfig) {
		existing, err := loadAgentConfig(installedConfig)
		if err != nil || existing.ID != cfg.ID {
			return errors.New("another pairing is installed; uninstall it first")
		}
	}
	if err := os.MkdirAll(installedDir, 0o700); err != nil {
		return err
	}
	if err := writePrivateJSON(installedConfig, cfg); err != nil {
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
		out, err := os.OpenFile(installedBinary+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
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
		if err := os.Rename(installedBinary+".tmp", installedBinary); err != nil {
			return err
		}
	}
	plan := makeStartupPlan(manager, directory)
	if err := writePrivateJSON(installedManifest, plan); err != nil {
		return err
	}
	for _, file := range plan.Files {
		if err := os.MkdirAll(filepath.Dir(file.Path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(file.Path, []byte(file.Content), file.Mode); err != nil {
			return err
		}
	}
	if manager == "runit" {
		ready := filepath.Join(directory, "rescue-agent", "supervise", "ok")
		deadline := time.Now().Add(10 * time.Second)
		for !exists(ready) {
			if time.Now().After(deadline) {
				return errors.New("runsvdir did not start the Rescue supervisor")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	if err := runStartupCommands(plan.Start); err != nil {
		return fmt.Errorf("startup installation: %w; credentials retained at %s", err, installedConfig)
	}
	fmt.Fprintf(commandStdout, "Paired %s; starts at boot using %s\n", cfg.Name, manager)
	return nil
}

func uninstallAgent() error {
	if os.Geteuid() != 0 {
		return elevate([]string{"uninstall"})
	}
	data, err := os.ReadFile(installedManifest)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("no installed Rescue agent")
	}
	if err != nil {
		return err
	}
	var plan startupPlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return err
	}
	if err := runStartupCommands(plan.Stop); err != nil {
		return err
	}
	for _, file := range plan.Files {
		if err := os.Remove(file.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if plan.Manager == "runit" && len(plan.Files) > 0 {
		_ = os.RemoveAll(filepath.Dir(plan.Files[0].Path))
	}
	if plan.Manager == "s6" && len(plan.Files) > 0 {
		if err := os.Remove(filepath.Dir(plan.Files[0].Path)); err != nil {
			return err
		}
		if err := runStartupCommands([][]string{{"s6", "repository", "sync"}, {"s6", "set", "commit"}, {"s6", "live", "install"}}); err != nil {
			return err
		}
	}
	if plan.Manager == "systemd" {
		if err := runStartupCommands([][]string{{"systemctl", "daemon-reload"}}); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(installedDir); err != nil {
		return err
	}
	fmt.Println("Rescue agent removed; revoke its pairing on the operator computer")
	return nil
}
