// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/UserExistsError/conpty"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sys/windows"
)

func isAdministrator() bool { return windows.GetCurrentProcessToken().IsElevated() }
func consoleFlag(flags *flag.FlagSet) *uint {
	return flags.Uint("console", 0, "parent console for administrator access")
}
func watchTerminalSize(session *ssh.Session) func() { return func() {} }

func attachParentConsole(pid uint) error {
	if pid == 0 {
		return nil
	}
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	kernel.NewProc("FreeConsole").Call()
	result, _, err := kernel.NewProc("AttachConsole").Call(uintptr(pid))
	if result == 0 {
		return err
	}
	for _, item := range []struct {
		id     uint32
		target **os.File
		name   string
	}{
		{windows.STD_INPUT_HANDLE, &os.Stdin, "stdin"}, {windows.STD_OUTPUT_HANDLE, &os.Stdout, "stdout"}, {windows.STD_ERROR_HANDLE, &os.Stderr, "stderr"},
	} {
		handle, err := windows.GetStdHandle(item.id)
		if err != nil {
			return err
		}
		*item.target = os.NewFile(uintptr(handle), item.name)
	}
	return nil
}

func currentSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func pairedIdentity(root, persistent bool) (AgentConfig, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return AgentConfig{}, err
	}
	account, domain, _, err := tokenUser.User.Sid.LookupAccount("")
	if err != nil {
		return AgentConfig{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return AgentConfig{}, err
	}
	shell, err := exec.LookPath("powershell.exe")
	if err != nil {
		return AgentConfig{}, err
	}
	cfg := AgentConfig{User: domain + `\` + account, SID: tokenUser.User.Sid.String(), UID: -1, GID: -1, Home: home, Shell: shell, Root: isAdministrator(), Persistent: persistent}
	if root && persistent {
		cfg.SID = "S-1-5-18"
		system, err := windows.StringToSid(cfg.SID)
		if err != nil {
			return AgentConfig{}, err
		}
		account, domain, _, err = system.LookupAccount("")
		if err != nil {
			return AgentConfig{}, err
		}
		cfg.User = domain + `\` + account
		cfg.Home = filepath.Join(os.Getenv("SystemRoot"), "System32", "config", "systemprofile")
	}
	return cfg, nil
}

func checkAgentIdentity(cfg AgentConfig) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	if cfg.SID != sid {
		return fmt.Errorf("agent must run as %s", cfg.User)
	}
	if cfg.Root != (isAdministrator() || sid == "S-1-5-18") {
		return errors.New("agent permissions do not match the pairing")
	}
	return nil
}

func protectPrivateFile(path string) error {
	sid, err := currentSID()
	if err != nil {
		return err
	}
	return setWindowsACL(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+sid+")")
}

func setWindowsACL(path, acl string) error {
	sd, err := windows.SecurityDescriptorFromString(acl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func sessionCommand(shell, text string, interactive bool) *exec.Cmd {
	if interactive {
		return exec.Command(shell, "-NoLogo", "-NoProfile")
	}
	script := "[Console]::OutputEncoding = New-Object Text.UTF8Encoding($false); $OutputEncoding = [Console]::OutputEncoding; " + text + "; if ($?) { exit 0 }; if ($LASTEXITCODE) { exit $LASTEXITCODE }; exit 1"
	return exec.Command(shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", powershellEncoded(script))
}

func prepareCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW}
}
func stopCommand(cmd *exec.Cmd) {
	if cmd.Process != nil {
		kill := exec.Command("taskkill.exe", "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F")
		kill.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW}
		_ = kill.Run()
	}
}
func signalCommand(cmd *exec.Cmd, name string) error {
	if name == "TERM" || name == "KILL" {
		stopCommand(cmd)
		return nil
	}
	return fmt.Errorf("signal %s requires an interactive Windows terminal", name)
}

type windowsTerminal struct {
	*conpty.ConPty
	cmd      *exec.Cmd
	once     sync.Once
	closeErr error
}

func startTerminal(cmd *exec.Cmd, size *terminalRequest) (agentTerminal, error) {
	if size.Columns > 32767 || size.Rows > 32767 {
		return nil, errors.New("Windows terminal dimensions exceed 32767")
	}
	words := make([]string, len(cmd.Args))
	for i, word := range cmd.Args {
		words[i] = syscall.EscapeArg(word)
	}
	console, err := conpty.Start(strings.Join(words, " "), conpty.ConPtyDimensions(int(size.Columns), int(size.Rows)), conpty.ConPtyWorkDir(cmd.Dir), conpty.ConPtyEnv(cmd.Env))
	if err != nil {
		return nil, err
	}
	cmd.Process, err = os.FindProcess(console.Pid())
	if err != nil {
		console.Close()
		return nil, err
	}
	return &windowsTerminal{ConPty: console, cmd: cmd}, nil
}
func (terminal *windowsTerminal) Wait() (uint32, error) {
	return terminal.ConPty.Wait(context.Background())
}
func (terminal *windowsTerminal) Resize(columns, rows uint32) error {
	if columns > 32767 || rows > 32767 {
		return errors.New("Windows terminal dimensions exceed 32767")
	}
	return terminal.ConPty.Resize(int(columns), int(rows))
}
func (terminal *windowsTerminal) Signal(name string) error {
	if name == "INT" || name == "QUIT" {
		_, err := terminal.Write([]byte{3})
		return err
	}
	return signalCommand(terminal.cmd, name)
}
func (terminal *windowsTerminal) Close() error {
	terminal.once.Do(func() { terminal.closeErr = terminal.ConPty.Close(); terminal.cmd.Process.Release() })
	return terminal.closeErr
}

type shellExecuteInfo struct {
	Size, Mask                        uint32
	Window                            windows.Handle
	Verb, File, Parameters, Directory *uint16
	Show                              int32
	Instance                          windows.Handle
	IDList                            uintptr
	Class                             *uint16
	ClassKey                          windows.Handle
	HotKey                            uint32
	Icon, Process                     windows.Handle
}

func elevate(args []string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	words := make([]string, 0, len(args)+2)
	for _, arg := range args {
		words = append(words, syscall.EscapeArg(arg))
	}
	words = append(words, "--console", fmt.Sprint(os.Getpid()))
	verb, _ := windows.UTF16PtrFromString("runas")
	file, err := windows.UTF16PtrFromString(binary)
	if err != nil {
		return err
	}
	parameters, err := windows.UTF16PtrFromString(strings.Join(words, " "))
	if err != nil {
		return err
	}
	info := shellExecuteInfo{Mask: 0x40 | 0x8000, Verb: verb, File: file, Parameters: parameters, Show: 0}
	info.Size = uint32(unsafe.Sizeof(info))
	result, _, callErr := windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW").Call(uintptr(unsafe.Pointer(&info)))
	if result == 0 {
		return fmt.Errorf("administrator approval: %w", callErr)
	}
	defer windows.CloseHandle(info.Process)
	if _, err := windows.WaitForSingleObject(info.Process, windows.INFINITE); err != nil {
		return err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.Process, &code); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("administrator process exited with code %d", code)
	}
	return nil
}
