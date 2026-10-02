//go:build !windows

// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"flag"
	"fmt"
	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

func isAdministrator() bool                 { return os.Geteuid() == 0 }
func consoleFlag(flags *flag.FlagSet) *uint { return new(uint) }
func attachParentConsole(pid uint) error    { return nil }
func protectPrivateFile(path string) error  { return nil }

func pairedIdentity(root, persistent bool) (AgentConfig, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return AgentConfig{}, err
	}
	username, err := exec.Command("id", "-un").Output()
	if err != nil {
		return AgentConfig{}, err
	}
	groups, err := os.Getgroups()
	if err != nil {
		return AgentConfig{}, err
	}
	return AgentConfig{User: strings.TrimSpace(string(username)), UID: os.Getuid(), GID: os.Getgid(), Groups: groups, Home: home, Shell: getEnv("SHELL", "/bin/sh"), Root: isAdministrator(), Persistent: persistent}, nil
}

func checkAgentIdentity(cfg AgentConfig) error {
	if isAdministrator() && cfg.UID != 0 {
		if err := dropAgentPrivileges(cfg); err != nil {
			return err
		}
	}
	if os.Geteuid() != cfg.UID {
		return fmt.Errorf("agent must run as %s", cfg.User)
	}
	return nil
}

func sessionCommand(shell, text string, interactive bool) *exec.Cmd {
	if interactive {
		return exec.Command(shell, "-i")
	}
	return exec.Command(shell, "-c", text)
}

type unixTerminal struct {
	*os.File
	cmd *exec.Cmd
}

func startTerminal(cmd *exec.Cmd, size *terminalRequest) (agentTerminal, error) {
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(size.Rows), Cols: uint16(size.Columns)})
	if err != nil {
		return nil, err
	}
	return &unixTerminal{File: file, cmd: cmd}, nil
}
func (terminal *unixTerminal) Wait() (uint32, error) {
	return sessionExitStatus(terminal.cmd.Wait()), nil
}
func (terminal *unixTerminal) Resize(columns, rows uint32) error {
	return pty.Setsize(terminal.File, &pty.Winsize{Rows: uint16(rows), Cols: uint16(columns)})
}
func (terminal *unixTerminal) Signal(name string) error { return signalCommand(terminal.cmd, name) }

func prepareCommand(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func dropAgentPrivileges(cfg AgentConfig) error {
	if err := syscall.Setgroups(cfg.Groups); err != nil {
		return err
	}
	if err := syscall.Setgid(cfg.GID); err != nil {
		return err
	}
	return syscall.Setuid(cfg.UID)
}

func stopCommand(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }

func signalCommand(cmd *exec.Cmd, name string) error {
	signals := map[string]syscall.Signal{"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "KILL": syscall.SIGKILL, "TERM": syscall.SIGTERM, "USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2, "STOP": syscall.SIGSTOP, "CONT": syscall.SIGCONT}
	signal, ok := signals[name]
	if !ok {
		return fmt.Errorf("unsupported signal %s", name)
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}

func watchTerminalSize(session *ssh.Session) func() {
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-changes:
				if width, height, err := term.GetSize(int(os.Stdin.Fd())); err == nil {
					session.WindowChange(height, width)
				}
			}
		}
	}()
	return func() { signal.Stop(changes); close(done) }
}
