// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type AgentConfig struct {
	URL         string `json:"url"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	OperatorKey string `json:"operatorKey"`
	PrivateKey  string `json:"privateKey"`
	User        string `json:"user"`
	UID         int    `json:"uid"`
	GID         int    `json:"gid"`
	Groups      []int  `json:"groups"`
	SID         string `json:"sid,omitempty"`
	Home        string `json:"home"`
	Shell       string `json:"shell"`
	Root        bool   `json:"root"`
	Persistent  bool   `json:"persistent"`
}

func websocketURL(base, path string) string {
	u, _ := url.Parse(base)
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.Path = path
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

func runAgent(cfg AgentConfig) error {
	if err := os.Chdir(cfg.Home); err != nil {
		return err
	}
	signer, err := ssh.ParsePrivateKey([]byte(cfg.PrivateKey))
	if err != nil {
		return err
	}
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if keyText(key) != cfg.OperatorKey {
			return nil, errors.New("operator key rejected")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(signer)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("%s: %s access, connecting to %s", cfg.Name, cfg.User, cfg.URL)
	for ctx.Err() == nil {
		err = agentConnection(ctx, cfg, serverConfig)
		if ctx.Err() != nil {
			break
		}
		log.Printf("Disconnected: %v; retrying in 3 seconds", err)
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	return nil
}

func agentConnection(ctx context.Context, cfg AgentConfig, config *ssh.ServerConfig) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	ws, response, err := websocket.Dial(dialCtx, websocketURL(cfg.URL, "/remote/connect/"+cfg.ID), nil)
	cancel()
	if err != nil {
		if response != nil && response.StatusCode == http.StatusForbidden {
			return errors.New("pairing was revoked; pair again to restore access")
		}
		return err
	}
	defer ws.CloseNow()
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	defer server.Close()
	log.Printf("Connected as %s", cfg.Name)
	go func() {
		for request := range requests {
			if request.WantReply {
				request.Reply(request.Type == "keepalive@openssh.com", nil)
			}
		}
	}()
	// Detect dead connections even when no command is running.
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := ws.Ping(pingCtx)
				cancel()
				if err != nil {
					server.Close()
					return
				}
			}
		}
	}()
	for channel := range channels {
		switch channel.ChannelType() {
		case "session":
			go serveAgentSession(channel, cfg)
		case "direct-tcpip":
			go serveAgentForward(channel)
		default:
			channel.Reject(ssh.UnknownChannelType, "unsupported channel")
		}
	}
	return server.Wait()
}

type terminalRequest struct {
	Term    string
	Columns uint32
	Rows    uint32
	Width   uint32
	Height  uint32
	Modes   string
}

func serveAgentSession(in ssh.NewChannel, cfg AgentConfig) {
	channel, requests, err := in.Accept()
	if err != nil {
		return
	}
	defer channel.Close()
	var terminal *terminalRequest
	env := append(os.Environ(), "HOME="+cfg.Home, "USER="+cfg.User, "LOGNAME="+cfg.User, "SHELL="+cfg.Shell)
	for request := range requests {
		ok := false
		switch request.Type {
		case "env":
			var value struct{ Name, Value string }
			if ssh.Unmarshal(request.Payload, &value) == nil && value.Name != "" && !strings.ContainsAny(value.Name, "=\x00") && !strings.ContainsRune(value.Value, '\x00') {
				env = append(env, value.Name+"="+value.Value)
				ok = true
			}
		case "pty-req":
			var value terminalRequest
			if ssh.Unmarshal(request.Payload, &value) == nil && value.Columns <= 65535 && value.Rows <= 65535 {
				terminal = &value
				env = append(env, "TERM="+value.Term)
				ok = true
			}
		case "subsystem":
			var value struct{ Name string }
			if ssh.Unmarshal(request.Payload, &value) == nil && value.Name == "sftp" {
				request.Reply(true, nil)
				var options []sftp.ServerOption
				if runtime.GOOS == "windows" {
					options = append(options, sftp.WindowsRootEnumeratesDrives())
				}
				server, err := sftp.NewServer(channel, options...)
				if err != nil {
					fmt.Fprintln(channel.Stderr(), err)
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				if err = server.Serve(); err != nil && !errors.Is(err, io.EOF) {
					fmt.Fprintln(channel.Stderr(), err)
				}
				return
			}
		case "exec", "shell":
			commandText := ""
			if request.Type == "exec" {
				var command struct{ Command string }
				if ssh.Unmarshal(request.Payload, &command) != nil {
					request.Reply(false, nil)
					continue
				}
				commandText = command.Command
			}
			cmd := sessionCommand(cfg.Shell, commandText, request.Type == "shell")
			cmd.Env, cmd.Dir = env, cfg.Home
			runSessionCommand(channel, requests, request, cmd, terminal)
			return
		}
		if request.WantReply {
			request.Reply(ok, nil)
		}
	}
}

type agentTerminal interface {
	io.ReadWriteCloser
	Wait() (uint32, error)
	Resize(uint32, uint32) error
	Signal(string) error
}

func sessionExitStatus(err error) uint32 {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() >= 0 {
		return uint32(exit.ExitCode())
	}
	return 1
}

func runSessionCommand(channel ssh.Channel, requests <-chan *ssh.Request, start *ssh.Request, cmd *exec.Cmd, terminal *terminalRequest) {
	var tty agentTerminal
	var err error
	var outputDone chan struct{}
	if terminal != nil {
		tty, err = startTerminal(cmd, terminal)
		if err == nil {
			defer tty.Close()
			outputDone = make(chan struct{})
			go func() { io.Copy(channel, tty); close(outputDone) }()
			go func() { io.Copy(tty, channel) }()
		}
	} else {
		prepareCommand(cmd)
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			start.Reply(false, nil)
			fmt.Fprintln(channel.Stderr(), pipeErr)
			return
		}
		cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
		// A child retaining a pipe must not keep a finished session open forever.
		cmd.WaitDelay = 2 * time.Second
		err = cmd.Start()
		if err == nil {
			go func() { io.Copy(stdin, channel); stdin.Close() }()
		} else {
			stdin.Close()
		}
	}
	if err != nil {
		start.Reply(false, nil)
		fmt.Fprintln(channel.Stderr(), err)
		return
	}
	start.Reply(true, nil)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case request, open := <-requests:
				if !open {
					select {
					case <-done:
						return
					default:
					}
					stopCommand(cmd)
					return
				}
				ok := false
				switch request.Type {
				case "window-change":
					var size struct{ Columns, Rows, Width, Height uint32 }
					if tty != nil && ssh.Unmarshal(request.Payload, &size) == nil && size.Columns <= 65535 && size.Rows <= 65535 {
						ok = tty.Resize(size.Columns, size.Rows) == nil
					}
				case "signal":
					var value struct{ Signal string }
					if ssh.Unmarshal(request.Payload, &value) == nil {
						if tty != nil {
							ok = tty.Signal(value.Signal) == nil
						} else {
							ok = signalCommand(cmd, value.Signal) == nil
						}
					}
				}
				if request.WantReply {
					request.Reply(ok, nil)
				}
			}
		}
	}()
	status := uint32(0)
	if tty != nil {
		status, err = tty.Wait()
	} else {
		err = cmd.Wait()
		status = sessionExitStatus(err)
	}
	close(done)
	if outputDone != nil {
		select {
		case <-outputDone:
		case <-time.After(2 * time.Second):
			tty.Close()
			<-outputDone
		}
	}
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		status = sessionExitStatus(err)
	}
	channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

func serveAgentForward(in ssh.NewChannel) {
	var address struct {
		Host       string
		Port       uint32
		Origin     string
		OriginPort uint32
	}
	if err := ssh.Unmarshal(in.ExtraData(), &address); err != nil || address.Port > 65535 {
		in.Reject(ssh.ConnectionFailed, "invalid address")
		return
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(address.Host, fmt.Sprint(address.Port)), 10*time.Second)
	if err != nil {
		in.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	defer conn.Close()
	channel, requests, err := in.Accept()
	if err != nil {
		return
	}
	defer channel.Close()
	go ssh.DiscardRequests(requests)
	done := make(chan struct{})
	go func() {
		io.Copy(conn, channel)
		if tcp, ok := conn.(*net.TCPConn); ok {
			tcp.CloseWrite()
		}
		close(done)
	}()
	io.Copy(channel, conn)
	channel.CloseWrite()
	<-done
}

func loadAgentConfig(path string) (AgentConfig, error) {
	var cfg AgentConfig
	data, err := os.ReadFile(filepath.Clean(path))
	if err == nil {
		err = json.Unmarshal(data, &cfg)
	}
	if err == nil {
		u, parseErr := url.Parse(cfg.URL)
		if parseErr != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || cfg.ID == "" || cfg.Shell == "" {
			err = errors.New("invalid agent configuration")
		}
	}
	return cfg, err
}

func pairAgent(base, invite, operatorKey string, root, persistent bool) error {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("invalid Rescue URL")
	}
	if _, _, _, _, err := ssh.ParseAuthorizedKey([]byte(operatorKey)); err != nil {
		return errors.New("invalid operator key")
	}
	if root && !isAdministrator() {
		return elevate(os.Args[1:])
	}
	if persistent {
		if err := checkStartupSupport(); err != nil {
			return err
		}
	}
	cfg, err := pairedIdentity(root, persistent)
	if err != nil {
		return err
	}
	private, signer, err := newSSHKey()
	if err != nil {
		return err
	}
	cfg.URL, cfg.OperatorKey, cfg.PrivateKey = strings.TrimRight(base, "/"), operatorKey, private
	// Running from a root terminal grants root access, so the invitation must agree.
	if cfg.Root != root {
		return errors.New("this invitation requests user access; run it from a non-root account or select Root on the page")
	}
	target := Target{User: cfg.User, OS: runtime.GOOS, Arch: runtime.GOARCH, Root: cfg.Root, Persistent: persistent, PublicKey: keyText(signer.PublicKey())}
	data, _ := json.Marshal(target)
	req, err := http.NewRequest(http.MethodPost, cfg.URL+"/api/pairing/claim", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+invite)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 8192))
		return fmt.Errorf("pairing: %s", strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(response.Body).Decode(&target); err != nil {
		return err
	}
	cfg.ID, cfg.Name = target.ID, target.Name
	if persistent {
		dir, err := os.MkdirTemp("", "rescue-install-")
		if err != nil {
			return err
		}
		path := filepath.Join(dir, "agent.json")
		if err := writePrivateJSON(path, cfg); err != nil {
			return err
		}
		if isAdministrator() {
			err = installAgent(path)
		} else {
			args := []string{"install-agent", "--config", path, "--server", cfg.URL}
			if commandReport != nil {
				id, token := commandReport.credentials()
				if id != "" {
					args = append(args, "--log-id", id, "--log-token", token)
				}
			}
			err = elevate(args)
		}
		if err != nil {
			return fmt.Errorf("%w; retry with rescue install-agent --config %s", err, shellQuote(path))
		}
		os.RemoveAll(dir)
		return nil
	}
	return runAgent(cfg)
}
