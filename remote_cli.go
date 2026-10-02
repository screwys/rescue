// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func remoteCommand(command string) bool {
	switch command {
	case "pair", "agent", "install-agent", "uninstall", "startup", "targets", "exec", "shell", "ssh", "push", "pull", "wait", "revoke", "forward":
		return true
	}
	return false
}

func runRemoteCLI(args []string) int {
	code, err := remoteCLI(args)
	if err != nil {
		fmt.Fprintln(commandStderr, "rescue:", err)
		if code == 0 {
			code = 1
		}
	}
	if commandReport != nil {
		commandReport.Finish(code)
	}
	return code
}

func remoteCLI(args []string) (int, error) {
	command := args[0]
	flags := flag.NewFlagSet("rescue "+command, flag.ContinueOnError)
	var server, store, configPath, invite, operatorKey, listen, remote, manager, directory, after string
	var logID, logToken string
	var jsonOutput, wait, tty, root, persistent bool
	timeout := 2 * time.Minute
	console := consoleFlag(flags)
	switch command {
	case "pair":
		flags.StringVar(&logID, "log-id", "", "script run")
		flags.StringVar(&logToken, "log-token", "", "script log token")
		flags.StringVar(&server, "server", "", "Rescue URL from the invitation")
		flags.StringVar(&invite, "invite", "", "single-use invitation")
		flags.StringVar(&operatorKey, "operator-key", "", "operator public key")
		flags.BoolVar(&root, "root", false, "pair with root access")
		flags.BoolVar(&persistent, "persist", false, "start at boot")
	case "agent", "install-agent":
		flags.StringVar(&server, "server", "", "Rescue URL for logs")
		flags.StringVar(&logID, "log-id", "", "script run")
		flags.StringVar(&logToken, "log-token", "", "script log token")
		flags.StringVar(&configPath, "config", installedConfig, "agent configuration")
	case "startup":
		flags.StringVar(&manager, "manager", "", "startup manager to inspect; otherwise detect the running manager")
		flags.StringVar(&directory, "directory", "", "service directory for runit or s6")
	case "uninstall":
	default:
		flags.StringVar(&server, "server", "http://127.0.0.1:"+strconv.Itoa(getEnvInt("PORT", defaultPort)), "operator Rescue URL")
		if command != "targets" && command != "revoke" && command != "wait" {
			flags.StringVar(&store, "file", getEnv("RESCUE_FILE", ""), "operator TOML file; otherwise use the running server's file")
		}
		if command == "targets" || command == "wait" {
			flags.BoolVar(&jsonOutput, "json", false, "print targets as JSON")
		}
		if command != "targets" && command != "revoke" {
			flags.DurationVar(&timeout, "timeout", timeout, "reconnection wait timeout")
			if command != "wait" {
				flags.BoolVar(&wait, "wait", false, "wait for the target to connect")
			}
		}
		if command == "wait" {
			flags.StringVar(&after, "after", "", "wait for a connection newer than this connection ID")
		}
		if command == "exec" {
			flags.BoolVar(&tty, "tty", false, "allocate a terminal")
		}
		if command == "forward" {
			flags.StringVar(&listen, "listen", "127.0.0.1:8080", "local forwarding address")
			flags.StringVar(&remote, "remote", "127.0.0.1:8080", "target forwarding address")
		}
	}
	flags.Usage = func() {
		fmt.Fprintf(flags.Output(), "Usage: rescue %s [options] [target] [-- command | source destination]\n", command)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 1, err
	}
	positional := flags.Args()
	if err := attachParentConsole(*console); err != nil {
		return 1, err
	}
	switch command {
	case "pair":
		startRunReport(server, "Pairing", logID, logToken)
		return 0, pairAgent(server, invite, operatorKey, root, persistent)
	case "agent":
		startRunReport(server, "Agent", logID, logToken)
		cfg, err := loadAgentConfig(configPath)
		if err != nil {
			return 1, err
		}
		if commandReport == nil {
			startRunReport(cfg.URL, "Agent "+cfg.Name, "", "")
		}
		if err := checkAgentIdentity(cfg); err != nil {
			return 1, err
		}
		return 0, runAgent(cfg)
	case "install-agent":
		startRunReport(server, "Pairing", logID, logToken)
		if !isAdministrator() {
			return 0, elevate(args)
		}
		return 0, installAgent(configPath)
	case "uninstall":
		return 0, uninstallAgent()
	case "startup":
		if manager == "" {
			var err error
			manager, directory, err = startupManager()
			if err != nil {
				return 1, err
			}
		}
		plan := makeStartupPlan(manager, directory)
		if len(plan.Files) == 0 {
			return 1, errors.New("unknown startup manager")
		}
		if (manager == "runit" || manager == "s6") && !filepath.IsAbs(directory) {
			return 1, errors.New("--directory must be an absolute path")
		}
		return 0, json.NewEncoder(os.Stdout).Encode(plan)
	}
	u, err := localOperatorURL(server)
	if err != nil {
		return 1, err
	}
	server = u
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "targets" {
		targets, err := fetchTargets(ctx, server)
		if err != nil {
			return 1, err
		}
		if jsonOutput {
			return 0, json.NewEncoder(os.Stdout).Encode(targets)
		}
		for _, target := range targets {
			status := "offline"
			if target.Connected {
				status = "connected"
			}
			startup := "temporary"
			if target.Persistent {
				startup = "persistent"
			}
			fmt.Printf("%s\t%s\t%s/%s\t%s\t%s\t%s\n", target.Name, target.ID, target.OS, target.Arch, target.User, startup, status)
		}
		return 0, nil
	}
	if len(positional) == 0 {
		return 1, errors.New("target name or ID is required")
	}
	if command == "wait" {
		wait = true
	}
	resolveCtx := ctx
	if wait {
		var stop context.CancelFunc
		resolveCtx, stop = context.WithTimeout(ctx, timeout)
		defer stop()
	}
	target, err := resolveTarget(resolveCtx, server, positional[0], wait, after)
	if err != nil {
		return 1, err
	}
	if command == "wait" {
		if jsonOutput {
			return 0, json.NewEncoder(os.Stdout).Encode(target)
		}
		fmt.Fprintln(os.Stdout, target.Name)
		return 0, nil
	}
	if command == "revoke" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, server+"/api/targets/"+target.ID, nil)
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return 1, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			return 1, responseError(response)
		}
		return 0, nil
	}
	if !target.Connected {
		return 1, errors.New("target is offline; use --wait to wait for reconnection")
	}
	client, err := dialControl(ctx, server, store, target.ID)
	if err != nil {
		return 1, err
	}
	defer client.Close()
	switch command {
	case "exec", "shell", "ssh":
		words := positional[1:]
		if len(words) > 0 && words[0] == "--" {
			words = words[1:]
		}
		if command == "exec" && len(words) == 0 {
			return 1, errors.New("command is required after --")
		}
		return controlSession(ctx, client, words, tty || command != "exec", target.OS == "windows")
	case "push", "pull":
		if len(positional) != 3 {
			return 1, errors.New("source and destination are required")
		}
		source, destination := positional[1], positional[2]
		if target.OS == "windows" {
			if command == "push" {
				destination = windowsRemotePath(destination)
			} else {
				source = windowsRemotePath(source)
			}
		}
		return 0, transferFiles(client, command, source, destination)
	case "forward":
		return 0, forwardPort(ctx, client, listen, remote)
	}
	return 1, errors.New("unknown command")
}

func localOperatorURL(value string) (string, error) {
	// Command access is intentionally local to the computer running the operator.
	if !strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://") {
		return "", errors.New("--server must be an HTTP URL")
	}
	req, err := http.NewRequest(http.MethodGet, value, nil)
	if err != nil {
		return "", err
	}
	host := req.URL.Hostname()
	if host != "localhost" && !net.ParseIP(host).IsLoopback() {
		return "", errors.New("operator commands must connect through localhost")
	}
	return strings.TrimRight(value, "/"), nil
}

func fetchTargets(ctx context.Context, server string) ([]Target, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/targets", nil)
	if err != nil {
		return nil, err
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, responseError(response)
	}
	var targets []Target
	err = json.NewDecoder(response.Body).Decode(&targets)
	return targets, err
}

func responseError(response *http.Response) error {
	message, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
	return fmt.Errorf("%s: %s", response.Status, strings.TrimSpace(string(message)))
}

func resolveTarget(ctx context.Context, server, name string, wait bool, after string) (Target, error) {
	for {
		targets, err := fetchTargets(ctx, server)
		if err != nil {
			return Target{}, err
		}
		var found Target
		for _, target := range targets {
			if target.ID == name || target.Name == name {
				found = target
				break
			}
		}
		if found.ID == "" {
			return Target{}, fmt.Errorf("target %q is not paired", name)
		}
		if !wait || (found.Connected && found.Connection != after) {
			return found, nil
		}
		select {
		case <-ctx.Done():
			return Target{}, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func dialControl(ctx context.Context, server, store, id string) (*ssh.Client, error) {
	if store == "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/state", nil)
		if err != nil {
			return nil, err
		}
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.StatusCode != 200 {
			return nil, responseError(response)
		}
		var state APIState
		if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
			return nil, err
		}
		store = state.Server.File
	}
	data, err := os.ReadFile(pairingPath(store))
	if err != nil {
		return nil, fmt.Errorf("operator credentials: %w; use --file for the server's TOML file", err)
	}
	var state PairingState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey([]byte(state.PrivateKey))
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, response, err := websocket.Dial(dialCtx, websocketURL(server, "/remote/control/"+id), nil)
	cancel()
	if err != nil {
		if response != nil {
			return nil, fmt.Errorf("target connection: %s", response.Status)
		}
		return nil, err
	}
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	sshConn, channels, requests, err := ssh.NewClientConn(conn, id, &ssh.ClientConfig{User: "rescue", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: pinnedKey(keyText(signer.PublicKey()))})
	if err != nil {
		ws.CloseNow()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return ssh.NewClient(sshConn, channels, requests), nil
}

func controlSession(ctx context.Context, client *ssh.Client, words []string, terminal, windowsTarget bool) (int, error) {
	session, err := client.NewSession()
	if err != nil {
		return 1, err
	}
	defer session.Close()
	session.Stdin, session.Stdout, session.Stderr = os.Stdin, os.Stdout, os.Stderr
	var restore *term.State
	if terminal {
		width, height := 80, 24
		if term.IsTerminal(int(os.Stdin.Fd())) {
			width, height, err = term.GetSize(int(os.Stdin.Fd()))
			if err != nil {
				return 1, err
			}
			restore, err = term.MakeRaw(int(os.Stdin.Fd()))
			if err != nil {
				return 1, err
			}
			defer term.Restore(int(os.Stdin.Fd()), restore)
		}
		if err := session.RequestPty(getEnv("TERM", "xterm-256color"), height, width, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}); err != nil {
			return 1, err
		}
		stopResize := watchTerminalSize(session)
		defer stopResize()
	}
	if len(words) == 0 {
		err = session.Shell()
	} else {
		quoted := make([]string, len(words))
		for i, word := range words {
			quoted[i] = shellQuote(word)
			if windowsTarget {
				quoted[i] = powershellQuote(word)
			}
		}
		command := strings.Join(quoted, " ")
		if windowsTarget {
			command = "& " + command
		}
		err = session.Start(command)
	}
	if err != nil {
		return 1, err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			session.Signal(ssh.SIGTERM)
			client.Close()
		case <-done:
		}
	}()
	err = session.Wait()
	close(done)
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return exit.ExitStatus(), nil
	}
	if err != nil {
		return 1, err
	}
	return 0, nil
}

func windowsRemotePath(value string) string {
	if strings.HasPrefix(value, `\\`) {
		return value
	}
	if strings.HasPrefix(value, "//") {
		return strings.ReplaceAll(value, "/", `\`)
	}
	value = strings.ReplaceAll(value, `\`, "/")
	if len(value) >= 2 && value[1] == ':' {
		return "/" + value
	}
	return value
}

func transferFiles(client *ssh.Client, direction, source, destination string) error {
	files, err := sftp.NewClient(client)
	if err != nil {
		return err
	}
	defer files.Close()
	if direction == "push" {
		return filepath.WalkDir(source, func(local string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(source, local)
			if err != nil {
				return err
			}
			remote := path.Join(destination, filepath.ToSlash(relative))
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if err := files.MkdirAll(remote); err != nil {
					return err
				}
				return files.Chmod(remote, info.Mode().Perm())
			}
			if err := files.MkdirAll(path.Dir(remote)); err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				link, err := os.Readlink(local)
				if err != nil {
					return err
				}
				if _, err := files.Lstat(remote); err == nil {
					if err := files.Remove(remote); err != nil {
						return err
					}
				} else if !os.IsNotExist(err) {
					return err
				}
				return files.Symlink(link, remote)
			}
			in, err := os.Open(local)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := files.OpenFile(remote, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
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
			return files.Chmod(remote, info.Mode().Perm())
		})
	}
	walker := files.Walk(source)
	for walker.Step() {
		if walker.Err() != nil {
			return walker.Err()
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(walker.Path(), source), "/")
		local := filepath.Join(destination, filepath.FromSlash(relative))
		info := walker.Stat()
		if info.IsDir() {
			if err := os.MkdirAll(local, info.Mode().Perm()); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := files.ReadLink(walker.Path())
			if err != nil {
				return err
			}
			if _, err := os.Lstat(local); err == nil {
				if err := os.Remove(local); err != nil {
					return err
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := os.Symlink(link, local); err != nil {
				return err
			}
			continue
		}
		in, err := files.Open(walker.Path())
		if err != nil {
			return err
		}
		out, err := os.OpenFile(local, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			in.Close()
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		closeErr := out.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func forwardPort(ctx context.Context, client *ssh.Client, listen, remote string) error {
	if _, _, err := net.SplitHostPort(remote); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	go func() { <-ctx.Done(); listener.Close() }()
	fmt.Fprintf(os.Stderr, "%s -> %s\n", listener.Addr(), remote)
	for {
		local, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer local.Close()
			other, err := client.Dial("tcp", remote)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return
			}
			defer other.Close()
			done := make(chan struct{})
			go func() {
				io.Copy(other, local)
				if half, ok := other.(interface{ CloseWrite() error }); ok {
					half.CloseWrite()
				}
				close(done)
			}()
			io.Copy(local, other)
			if tcp, ok := local.(*net.TCPConn); ok {
				tcp.CloseWrite()
			}
			<-done
		}()
	}
}
