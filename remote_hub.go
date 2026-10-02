// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/crypto/ssh"
)

// Release builds use their own tag when downloading another platform's binary.
var version = "dev"

type Target struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	User       string `json:"user"`
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Root       bool   `json:"root"`
	Persistent bool   `json:"persistent"`
	PublicKey  string `json:"publicKey"`
	Connected  bool   `json:"connected"`
	Connection string `json:"connection,omitempty"`
}

type PairingState struct {
	PrivateKey string   `json:"privateKey"`
	Targets    []Target `json:"targets"`
}

type PairingOptions struct {
	Platform   string `json:"platform"`
	Name       string `json:"name"`
	Root       bool   `json:"root"`
	Persistent bool   `json:"persistent"`
	URL        string `json:"url"`
	LogID      string `json:"-"`
	LogToken   string `json:"-"`
}

type invitation struct {
	Options  PairingOptions
	Expires  time.Time
	TargetID string
}

type RemoteHub struct {
	mu      sync.Mutex
	path    string
	assets  string
	state   PairingState
	signer  ssh.Signer
	invites map[string]invitation
	clients map[string]*targetConnection
	ctx     context.Context
	cancel  context.CancelFunc
	logs    *RunLogs
}

type targetConnection struct {
	Client *ssh.Client
	ID     string
}

func pairingPath(storePath string) string { return storePath + ".pairing.json" }

func newSSHKey() (string, ssh.Signer, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "Rescue")
	if err != nil {
		return "", nil, err
	}
	encoded := string(pem.EncodeToMemory(block))
	signer, err := ssh.ParsePrivateKey([]byte(encoded))
	return encoded, signer, err
}

func randomID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func keyText(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

func pinnedKey(expected string) ssh.HostKeyCallback {
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if keyText(key) != expected {
			return errors.New("SSH identity does not match the paired machine")
		}
		return nil
	}
}

func writePrivateJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".rescue-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(data)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := protectPrivateFile(f.Name()); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func OpenRemoteHub(storePath, assets string) (*RemoteHub, error) {
	ctx, cancel := context.WithCancel(context.Background())
	h := &RemoteHub{path: pairingPath(storePath), assets: assets, invites: make(map[string]invitation), clients: make(map[string]*targetConnection), ctx: ctx, cancel: cancel}
	data, err := os.ReadFile(h.path)
	if err == nil {
		err = json.Unmarshal(data, &h.state)
	} else if errors.Is(err, os.ErrNotExist) {
		h.state.PrivateKey, h.signer, err = newSSHKey()
		if err == nil {
			err = writePrivateJSON(h.path, h.state)
		}
	}
	if err == nil {
		h.signer, err = ssh.ParsePrivateKey([]byte(h.state.PrivateKey))
	}
	if err != nil {
		cancel()
		return nil, fmt.Errorf("pairing state: %w", err)
	}
	return h, nil
}

func (h *RemoteHub) Close() {
	h.cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, client := range h.clients {
		client.Client.Close()
	}
}

func (h *RemoteHub) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/targets", h.listTargets)
	mux.HandleFunc("DELETE /api/targets/{id}", h.revokeTarget)
	mux.HandleFunc("POST /api/pairing", h.createInvitation)
	mux.HandleFunc("POST /api/pairing/claim", h.claimInvitation)
	mux.HandleFunc("GET /remote/bootstrap/{invite}", h.bootstrap)
	mux.HandleFunc("GET /remote/binary/{os}/{arch}", h.binary)
	mux.HandleFunc("GET /remote/connect/{id}", h.connectTarget)
	mux.HandleFunc("GET /remote/control/{id}", h.controlTarget)
}

func sameOriginMutation(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			http.Error(w, "origin rejected", http.StatusForbidden)
			return false
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "origin rejected", http.StatusForbidden)
		return false
	}
	return true
}

func isLoopback(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	return err == nil && net.ParseIP(host).IsLoopback()
}

func (h *RemoteHub) listTargets(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	list := append([]Target{}, h.state.Targets...)
	for i := range list {
		list[i].Connected = h.clients[list[i].ID] != nil
		if connection := h.clients[list[i].ID]; connection != nil {
			list[i].Connection = connection.ID
		}
	}
	h.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, list)
}

func (h *RemoteHub) revokeTarget(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(w, r) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	id := r.PathValue("id")
	next := h.state
	next.Targets = slices.DeleteFunc(append([]Target{}, h.state.Targets...), func(t Target) bool { return t.ID == id })
	if len(next.Targets) == len(h.state.Targets) {
		http.NotFound(w, r)
		return
	}
	if err := writePrivateJSON(h.path, next); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h.state = next
	for token, invite := range h.invites {
		if invite.TargetID == id {
			delete(h.invites, token)
		}
	}
	if client := h.clients[id]; client != nil {
		client.Client.Close()
		delete(h.clients, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *RemoteHub) createInvitation(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(w, r) {
		return
	}
	var options PairingOptions
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&options); err != nil {
		http.Error(w, "invalid pairing options", 400)
		return
	}
	u, err := url.Parse(options.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		http.Error(w, "invalid Rescue URL", 400)
		return
	}
	options.URL = strings.TrimRight(options.URL, "/")
	if options.Platform == "" {
		options.Platform = "unix"
	}
	if options.Platform != "unix" && options.Platform != "windows" {
		http.Error(w, "invalid target platform", 400)
		return
	}
	options.Name = strings.TrimSpace(options.Name)
	if len(options.Name) > 128 {
		http.Error(w, "name is too long", 400)
		return
	}
	token := randomID()
	h.mu.Lock()
	for key, invite := range h.invites {
		if time.Now().After(invite.Expires) {
			delete(h.invites, key)
		}
	}
	h.invites[token] = invitation{Options: options, Expires: time.Now().Add(15 * time.Minute)}
	h.mu.Unlock()
	command := "sh -c " + shellQuote(`if command -v curl >/dev/null 2>&1; then curl -fsSL "$1"; elif command -v wget >/dev/null 2>&1; then wget -qO- "$1"; elif command -v fetch >/dev/null 2>&1; then fetch -q -o - "$1"; else echo 'Install curl, wget, or fetch to pair this machine' >&2; exit 1; fi`) + " rescue " + shellQuote(options.URL+"/remote/bootstrap/"+token) + " | sh"
	if options.Platform == "windows" {
		command = "Invoke-RestMethod " + powershellQuote(options.URL+"/remote/bootstrap/"+token) + " | Invoke-Expression"
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"command": command})
}

func (h *RemoteHub) invite(token string) (invitation, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	invite, ok := h.invites[token]
	return invite, ok && time.Now().Before(invite.Expires)
}

func (h *RemoteHub) claimInvitation(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var target Target
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&target); err != nil {
		http.Error(w, "invalid target", 400)
		return
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(target.PublicKey))
	if err != nil {
		http.Error(w, "invalid target key", 400)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	invite, ok := h.invites[token]
	if !ok || time.Now().After(invite.Expires) {
		http.Error(w, "invitation expired or already used", 403)
		return
	}
	if target.Root != invite.Options.Root || target.Persistent != invite.Options.Persistent {
		http.Error(w, "target permissions do not match the invitation", 403)
		return
	}
	target.ID = invite.TargetID
	if target.ID == "" {
		target.ID = randomID()
	}
	target.PublicKey = keyText(key)
	target.Name = invite.Options.Name
	if target.Name == "" {
		target.Name = target.OS + "-" + target.ID[:8]
	}
	for _, existing := range h.state.Targets {
		if existing.Name == target.Name && existing.ID != target.ID {
			http.Error(w, "target name is already paired", 409)
			return
		}
	}
	target.Connected = false
	next := h.state
	next.Targets = append([]Target{}, h.state.Targets...)
	if invite.TargetID == "" {
		next.Targets = append(next.Targets, target)
	} else {
		index := slices.IndexFunc(next.Targets, func(t Target) bool { return t.ID == target.ID })
		if index < 0 {
			http.Error(w, "pairing was revoked", 403)
			return
		}
		next.Targets[index] = target
	}
	if err := writePrivateJSON(h.path, next); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	h.state = next
	invite.TargetID = target.ID
	h.invites[token] = invite
	writeJSON(w, target)
}

func (h *RemoteHub) bootstrap(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("invite")
	invite, ok := h.invite(token)
	if !ok {
		http.Error(w, "invitation expired or already used", 403)
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if h.logs != nil {
		var err error
		invite.Options.LogID, invite.Options.LogToken, err = h.logs.Create("Pairing", r.RemoteAddr)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	_, _ = fmt.Fprint(w, bootstrapScript(invite.Options, token, keyText(h.signer.PublicKey())))
}

func (h *RemoteHub) binary(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.invite(r.URL.Query().Get("invite")); !ok {
		http.Error(w, "invitation expired or already used", 403)
		return
	}
	goos, arch := r.PathValue("os"), r.PathValue("arch")
	if !slices.Contains([]string{"linux", "freebsd", "darwin", "windows"}, goos) || !slices.Contains([]string{"amd64", "arm64", "386", "arm"}, arch) {
		http.Error(w, "no binary for this platform", 404)
		return
	}
	name := "rescue-" + goos + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	path := filepath.Join(h.assets, name)
	if goos == runtime.GOOS && arch == runtime.GOARCH {
		var err error
		path, err = os.Executable()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	} else if _, err := os.Stat(path); err != nil {
		if version != "dev" {
			http.Redirect(w, r, "https://github.com/screwys/rescue/releases/download/"+url.PathEscape(version)+"/"+name, http.StatusFound)
			return
		}
		http.Error(w, "target binary missing; run scripts/build-targets.sh or set --assets", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, path)
}

func (h *RemoteHub) connectTarget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mu.Lock()
	var target Target
	for _, t := range h.state.Targets {
		if t.ID == id {
			target = t
			break
		}
	}
	h.mu.Unlock()
	if target.ID == "" {
		http.Error(w, "target revoked or unknown", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	conn := websocket.NetConn(h.ctx, ws, websocket.MessageBinary)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	sshConn, channels, requests, err := ssh.NewClientConn(conn, id, &ssh.ClientConfig{User: "rescue", Auth: []ssh.AuthMethod{ssh.PublicKeys(h.signer)}, HostKeyCallback: pinnedKey(target.PublicKey)})
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	client := ssh.NewClient(sshConn, channels, requests)
	h.mu.Lock()
	// A revocation during the handshake must not restore access.
	if !slices.ContainsFunc(h.state.Targets, func(t Target) bool { return t.ID == id && t.PublicKey == target.PublicKey }) {
		h.mu.Unlock()
		client.Close()
		return
	}
	if previous := h.clients[id]; previous != nil {
		previous.Client.Close()
	}
	h.clients[id] = &targetConnection{Client: client, ID: randomID()}
	for token, invite := range h.invites {
		if invite.TargetID == id {
			delete(h.invites, token)
		}
	}
	h.mu.Unlock()
	client.Wait()
	h.mu.Lock()
	if current := h.clients[id]; current != nil && current.Client == client {
		delete(h.clients, id)
	}
	h.mu.Unlock()
}

func (h *RemoteHub) controlTarget(w http.ResponseWriter, r *http.Request) {
	if !isLoopback(r) {
		http.Error(w, "agent commands are only available on the operator computer", 403)
		return
	}
	h.mu.Lock()
	connection := h.clients[r.PathValue("id")]
	h.mu.Unlock()
	if connection == nil {
		http.Error(w, "target is offline", 409)
		return
	}
	client := connection.Client
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	conn := websocket.NetConn(h.ctx, ws, websocket.MessageBinary)
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if keyText(key) != keyText(h.signer.PublicKey()) {
			return nil, errors.New("operator key rejected")
		}
		return nil, nil
	}}
	config.AddHostKey(h.signer)
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for channel := range channels {
		go func(in ssh.NewChannel) {
			out, outRequests, err := client.OpenChannel(in.ChannelType(), in.ExtraData())
			if err != nil {
				in.Reject(ssh.ConnectionFailed, err.Error())
				return
			}
			incoming, inRequests, err := in.Accept()
			if err != nil {
				out.Close()
				return
			}
			bridgeSSHChannels(incoming, out, inRequests, outRequests)
		}(channel)
	}
}

func bridgeSSHChannels(a, b ssh.Channel, aRequests, bRequests <-chan *ssh.Request) {
	defer a.Close()
	defer b.Close()
	forwardRequests := func(requests <-chan *ssh.Request, dst ssh.Channel) {
		for request := range requests {
			ok, err := dst.SendRequest(request.Type, request.WantReply, request.Payload)
			if request.WantReply {
				request.Reply(err == nil && ok, nil)
			}
		}
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { forwardRequests(aRequests, b); b.Close() }()
	go func() { defer wg.Done(); forwardRequests(bRequests, a) }()
	go func() { defer wg.Done(); io.Copy(a, b); a.CloseWrite() }()
	go func() { io.Copy(b, a); b.CloseWrite() }()
	go func() { defer wg.Done(); io.Copy(a.Stderr(), b.Stderr()) }()
	go func() { io.Copy(b.Stderr(), a.Stderr()) }()
	wg.Wait()
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
