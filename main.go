// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"archive/zip"
	"bufio"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/pelletier/go-toml/v2"
)

//go:embed static/*
var embeddedFiles embed.FS

const (
	defaultHost = "0.0.0.0"
	defaultPort = 5000
	defaultFile = "rescue.toml"
)

type Config struct {
	Host string
	Port int
	File string
}

type StoreFile struct {
	Version      int           `toml:"version" json:"version"`
	Instructions []Instruction `toml:"instructions" json:"instructions"`
	Scripts      []Script      `toml:"scripts" json:"scripts"`
}

type Instruction struct {
	ID      string `toml:"id" json:"id"`
	Title   string `toml:"title" json:"title"`
	Content string `toml:"content" json:"content"`
}

type Script struct {
	ID       string `toml:"id" json:"id"`
	Filename string `toml:"filename" json:"filename"`
	Content  string `toml:"content" json:"content"`
}

type APIState struct {
	Version      int           `json:"version"`
	Instructions []Instruction `json:"instructions"`
	Scripts      []Script      `json:"scripts"`
	Server       ServerInfo    `json:"server"`
}

type ServerInfo struct {
	Host     string   `json:"host"`
	Port     int      `json:"port"`
	File     string   `json:"file"`
	ShareDir string   `json:"shareDir"`
	LocalURL string   `json:"localUrl"`
	LANURLs  []string `json:"lanUrls"`
}

type SharedFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

type Store struct {
	path string
	mu   sync.RWMutex
	data StoreFile
}

type Hub struct {
	mu      sync.Mutex
	clients map[chan struct{}]struct{}
}

type App struct {
	cfg       Config
	store     *Store
	hub       *Hub
	server    ServerInfo
	shareDir  string
	staticFS  http.Handler
	lastWrite atomic.Int64
}

func main() {
	cfg := parseConfig(os.Args[1:])
	if err := run(cfg); err != nil {
		log.Fatal(err)
	}
}

func parseConfig(args []string) Config {
	cfg := Config{
		Host: getEnv("HOST", defaultHost),
		File: getEnv("RESCUE_FILE", defaultFile),
	}
	cfg.Port = getEnvInt("PORT", defaultPort)

	flags := flag.NewFlagSet("rescue", flag.ExitOnError)
	flags.StringVar(&cfg.Host, "host", cfg.Host, "host/interface to bind")
	flags.IntVar(&cfg.Port, "port", cfg.Port, "port to listen on")
	flags.StringVar(&cfg.File, "file", cfg.File, "TOML file to read and save")
	flags.Parse(args)
	return cfg
}

func run(cfg Config) error {
	if cfg.Port < 1 || cfg.Port > 65535 {
		return fmt.Errorf("invalid port %d", cfg.Port)
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		if isAddrInUse(err) {
			yes, promptErr := promptKillBusyPort(cfg.Port, os.Stdin, os.Stdout)
			if promptErr != nil {
				return fmt.Errorf("port %d is already in use; choose another port with --port", cfg.Port)
			}
			if !yes {
				return fmt.Errorf("port %d is already in use", cfg.Port)
			}
			if err := killPort(cfg.Port); err != nil {
				return fmt.Errorf("could not clear port %d: %w", cfg.Port, err)
			}
			time.Sleep(250 * time.Millisecond)
			listener, err = net.Listen("tcp", addr)
			if err != nil {
				if isAddrInUse(err) {
					return fmt.Errorf("port %d is still in use after trying to clear it", cfg.Port)
				}
				return err
			}
		} else {
			return err
		}
	}
	defer listener.Close()

	store, err := OpenStore(cfg.File)
	if err != nil {
		return err
	}

	static, err := fs.Sub(embeddedFiles, "static")
	if err != nil {
		return err
	}
	shareDir := defaultShareDir(store.path)
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		return err
	}

	serverInfo := ServerInfo{
		Host:     cfg.Host,
		Port:     cfg.Port,
		File:     store.path,
		ShareDir: shareDir,
		LocalURL: fmt.Sprintf("http://127.0.0.1:%d", cfg.Port),
		LANURLs:  lanURLs(cfg.Port),
	}
	app := &App{
		cfg:      cfg,
		store:    store,
		hub:      NewHub(),
		server:   serverInfo,
		shareDir: shareDir,
		staticFS: http.FileServer(http.FS(static)),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.watchFile(ctx)

	mux := http.NewServeMux()
	app.routes(mux)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	printStartup(serverInfo)
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(listener)
	}()
	go openBrowser(serverInfo.LocalURL)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownErr := srv.Shutdown(shutdownCtx)
		if errors.Is(shutdownErr, context.DeadlineExceeded) {
			_ = srv.Close()
			shutdownErr = nil
		}
		serveErr := <-errCh
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(shutdownErr, serveErr)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (a *App) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /", a.handleIndex)
	mux.Handle("GET /static/", http.StripPrefix("/static/", a.staticFS))
	mux.HandleFunc("GET /api/state", a.handleGetState)
	mux.HandleFunc("PUT /api/state", a.handlePutState)
	mux.HandleFunc("POST /api/reset", a.handleReset)
	mux.HandleFunc("GET /api/files", a.handleListFiles)
	mux.HandleFunc("POST /api/files", a.handleUploadFiles)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /files.zip", a.handleDownloadZip)
	mux.HandleFunc("GET /files/{name}", a.handleDownloadFile)
	mux.HandleFunc("GET /{name}", a.handleScript)
}

func (a *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = "/"
	http.ServeFileFS(w, r, mustSub(embeddedFiles, "static"), "index.html")
}

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	a.staticFS.ServeHTTP(w, r)
}

func (a *App) handleGetState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.state())
}

func (a *App) handlePutState(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var next StoreFile
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&next); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := normalizeAndValidate(&next); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := a.store.Save(next); err != nil {
		http.Error(w, "save failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.lastWrite.Store(time.Now().UnixNano())
	a.hub.Broadcast()
	writeJSON(w, a.state())
}

func (a *App) handleReset(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Save(defaultStore()); err != nil {
		http.Error(w, "reset failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := clearSharedFiles(a.shareDir); err != nil {
		http.Error(w, "could not clear shared files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	a.lastWrite.Store(time.Now().UnixNano())
	a.hub.Broadcast()
	writeJSON(w, a.state())
}

func (a *App) handleListFiles(w http.ResponseWriter, r *http.Request) {
	files, err := listSharedFiles(a.shareDir)
	if err != nil {
		http.Error(w, "could not list files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, files)
}

func (a *App) handleUploadFiles(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		http.Error(w, "invalid upload: "+err.Error(), http.StatusBadRequest)
		return
	}
	parts := r.MultipartForm.File["files"]
	if len(parts) == 0 {
		http.Error(w, "choose at least one file", http.StatusBadRequest)
		return
	}
	for _, part := range parts {
		name := strings.TrimSpace(part.Filename)
		if !validSharedFilename(name) {
			http.Error(w, fmt.Sprintf("invalid filename %q", part.Filename), http.StatusBadRequest)
			return
		}
		src, err := part.Open()
		if err != nil {
			http.Error(w, "could not read upload: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := saveSharedFile(a.shareDir, name, src); err != nil {
			src.Close()
			http.Error(w, "could not save upload: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if err := src.Close(); err != nil {
			http.Error(w, "could not close upload: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	a.hub.Broadcast()
	w.WriteHeader(http.StatusCreated)
}

func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := a.hub.Subscribe()
	defer a.hub.Unsubscribe(ch)
	fmt.Fprint(w, "event: ready\ndata: {}\n\n")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			fmt.Fprint(w, "event: update\ndata: {}\n\n")
			flusher.Flush()
		}
	}
}

func (a *App) handleScript(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.PathValue("name"), "/")
	script, ok := a.store.ScriptByFilename(name)
	if !ok {
		http.NotFound(w, r)
		return
	}
	contentType := mime.TypeByExtension(filepath.Ext(script.Filename))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, script.Content)
}

func (a *App) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if !validSharedFilename(name) {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(a.shareDir, name)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}

func (a *App) handleDownloadZip(w http.ResponseWriter, r *http.Request) {
	files, err := listSharedFiles(a.shareDir)
	if err != nil {
		http.Error(w, "could not list files: "+err.Error(), http.StatusInternalServerError)
		return
	}
	files, err = selectedSharedFiles(files, r.URL.Query()["name"])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if len(files) == 0 {
		http.Error(w, "no shared files", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "rescue-files.zip"}))
	w.Header().Set("Cache-Control", "no-store")

	archive := zip.NewWriter(w)
	defer archive.Close()
	for _, file := range files {
		if err := addSharedFileToZip(archive, a.shareDir, file); err != nil {
			log.Printf("could not add shared file %q to zip: %v", file.Name, err)
			return
		}
	}
}

func selectedSharedFiles(files []SharedFile, names []string) ([]SharedFile, error) {
	if len(names) == 0 {
		return files, nil
	}
	byName := make(map[string]SharedFile, len(files))
	for _, file := range files {
		byName[file.Name] = file
	}
	seen := make(map[string]struct{}, len(names))
	selected := make([]SharedFile, 0, len(names))
	for _, name := range names {
		if !validSharedFilename(name) {
			return nil, fmt.Errorf("invalid filename %q", name)
		}
		if _, ok := seen[name]; ok {
			continue
		}
		file, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("unknown shared file %q", name)
		}
		seen[name] = struct{}{}
		selected = append(selected, file)
	}
	return selected, nil
}

func (a *App) state() APIState {
	data := a.store.Get()
	return APIState{
		Version:      data.Version,
		Instructions: data.Instructions,
		Scripts:      data.Scripts,
		Server:       a.server,
	}
}

func defaultShareDir(storePath string) string {
	return filepath.Join(filepath.Dir(storePath), "rescue-files")
}

func listSharedFiles(dir string) ([]SharedFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	files := make([]SharedFile, 0, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.IsDir() {
			continue
		}
		if !validSharedFilename(entry.Name()) {
			continue
		}
		files = append(files, SharedFile{
			Name:     entry.Name(),
			Size:     info.Size(),
			Modified: info.ModTime(),
		})
	}
	slices.SortFunc(files, func(a, b SharedFile) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return files, nil
}

func saveSharedFile(dir, name string, src io.Reader) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer dst.Close()
	_, err = io.Copy(dst, src)
	return err
}

func addSharedFileToZip(archive *zip.Writer, dir string, file SharedFile) error {
	path := filepath.Join(dir, file.Name)
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return err
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}
	header.Name = file.Name
	header.Method = zip.Deflate

	dst, err := archive.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, src)
	return err
}

func clearSharedFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) watchFile(ctx context.Context) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("file watching disabled: %v", err)
		return
	}
	defer watcher.Close()

	dir := filepath.Dir(a.store.path)
	if err := watcher.Add(dir); err != nil {
		log.Printf("file watching disabled: %v", err)
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case err := <-watcher.Errors:
			if err != nil {
				log.Printf("file watch error: %v", err)
			}
		case event := <-watcher.Events:
			if filepath.Clean(event.Name) != filepath.Clean(a.store.path) {
				continue
			}
			lastWrite := time.Unix(0, a.lastWrite.Load())
			if !lastWrite.IsZero() && time.Since(lastWrite) < 200*time.Millisecond {
				continue
			}
			if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) || event.Has(fsnotify.Rename) {
				if err := a.store.Reload(); err != nil {
					log.Printf("could not reload %s: %v", a.store.path, err)
					continue
				}
				a.hub.Broadcast()
			}
		}
	}
}

func OpenStore(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, err
		}
		if err := atomicWriteTOML(abs, defaultStore()); err != nil {
			return nil, err
		}
	}
	store := &Store{path: abs}
	if err := store.Reload(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var data StoreFile
	if err := toml.Unmarshal(raw, &data); err != nil {
		return err
	}
	if err := normalizeAndValidate(&data); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	return nil
}

func (s *Store) Save(data StoreFile) error {
	if err := normalizeAndValidate(&data); err != nil {
		return err
	}
	if err := atomicWriteTOML(s.path, data); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = data
	return nil
}

func (s *Store) Get() StoreFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return StoreFile{
		Version:      s.data.Version,
		Instructions: slices.Clone(s.data.Instructions),
		Scripts:      slices.Clone(s.data.Scripts),
	}
}

func (s *Store) ScriptByFilename(filename string) (Script, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, script := range s.data.Scripts {
		if script.Filename == filename {
			return script, true
		}
	}
	return Script{}, false
}

func defaultStore() StoreFile {
	return StoreFile{
		Version: 1,
		Instructions: []Instruction{
			{ID: "instruction-1", Title: "Instruction 1", Content: ""},
			{ID: "instruction-2", Title: "Instruction 2", Content: ""},
		},
		Scripts: []Script{
			{ID: "script-1", Filename: "install.sh", Content: ""},
			{ID: "script-2", Filename: "script-2.sh", Content: ""},
		},
	}
}

func normalizeAndValidate(data *StoreFile) error {
	if data.Version == 0 {
		data.Version = 1
	}
	if len(data.Instructions) == 0 {
		data.Instructions = defaultStore().Instructions
	}
	if len(data.Scripts) == 0 {
		data.Scripts = defaultStore().Scripts
	}

	seenScripts := map[string]struct{}{}
	for i := range data.Instructions {
		data.Instructions[i].ID = normalizeBlockID("instruction", data.Instructions[i].ID, i+1)
		if strings.TrimSpace(data.Instructions[i].Title) == "" {
			data.Instructions[i].Title = fmt.Sprintf("Instruction %d", i+1)
		}
	}
	for i := range data.Scripts {
		data.Scripts[i].ID = normalizeBlockID("script", data.Scripts[i].ID, i+1)
		filename := strings.TrimSpace(data.Scripts[i].Filename)
		if filename == "" {
			filename = fmt.Sprintf("script-%d.sh", i+1)
		}
		if !validScriptFilename(filename) {
			return fmt.Errorf("invalid script filename %q; use a simple relative filename like install.sh", filename)
		}
		if _, exists := seenScripts[filename]; exists {
			return fmt.Errorf("duplicate script filename %q", filename)
		}
		seenScripts[filename] = struct{}{}
		data.Scripts[i].Filename = filename
	}
	return nil
}

func normalizeBlockID(prefix, id string, index int) string {
	id = strings.TrimSpace(id)
	number, ok := strings.CutPrefix(id, prefix+"-")
	if ok {
		parsed, err := strconv.Atoi(number)
		if err == nil && parsed > 0 {
			return id
		}
	}
	return fmt.Sprintf("%s-%d", prefix, index)
}

func validScriptFilename(name string) bool {
	if name == "." || name == ".." || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			continue
		}
		if strings.ContainsRune("._-", r) {
			continue
		}
		return false
	}
	return true
}

func validSharedFilename(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if name != filepath.Base(name) || strings.Contains(name, "\\") {
		return false
	}
	for _, r := range name {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func atomicWriteTOML(path string, data StoreFile) error {
	raw, err := toml.Marshal(data)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	defer f.Close()
	_ = f.Sync()
}

func NewHub() *Hub {
	return &Hub{clients: map[chan struct{}]struct{}{}}
}

func (h *Hub) Subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[ch] = struct{}{}
	return ch
}

func (h *Hub) Unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, ch)
	close(ch)
}

func (h *Hub) Broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func getEnv(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func getEnvInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func isAddrInUse(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "address already in use") ||
		strings.Contains(strings.ToLower(err.Error()), "only one usage of each socket address")
}

func lanURLs(port int) []string {
	var urls []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return urls
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil || ip == nil {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			urls = append(urls, fmt.Sprintf("http://%s:%d", ip.String(), port))
		}
	}
	slices.Sort(urls)
	return slices.Compact(urls)
}

func printStartup(info ServerInfo) {
	log.Printf("rescue is listening")
	log.Printf("local: %s", info.LocalURL)
	for _, url := range info.LANURLs {
		log.Printf("lan:   %s", url)
	}
	log.Printf("file:  %s", info.File)
	log.Printf("warning: do not expose this server to the public internet")
}

func killPort(port int) error {
	commands := killPortCommands(runtime.GOOS, port)
	if len(commands) == 0 {
		return fmt.Errorf("clearing a busy port is unsupported on %s", runtime.GOOS)
	}
	var errs []error
	for _, args := range commands {
		if len(args) == 0 {
			continue
		}
		cmd := exec.Command(args[0], args[1:]...)
		output, err := cmd.CombinedOutput()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output))))
	}
	return errors.Join(errs...)
}

func openBrowser(url string) {
	time.Sleep(150 * time.Millisecond)
	for _, args := range openBrowserCommands(runtime.GOOS, url) {
		if len(args) == 0 {
			continue
		}
		if err := exec.Command(args[0], args[1:]...).Start(); err == nil {
			return
		}
	}
}

func openBrowserCommands(goos, url string) [][]string {
	switch goos {
	case "darwin":
		return [][]string{{"open", url}}
	case "windows":
		return [][]string{{"rundll32", "url.dll,FileProtocolHandler", url}}
	case "linux", "freebsd", "openbsd", "netbsd":
		return [][]string{
			{"xdg-open", url},
			{"gio", "open", url},
			{"sensible-browser", url},
		}
	default:
		return nil
	}
}

func promptKillBusyPort(port int, in io.Reader, out io.Writer) (bool, error) {
	if file, ok := in.(*os.File); ok {
		stat, err := file.Stat()
		if err != nil {
			return false, err
		}
		if stat.Mode()&os.ModeCharDevice == 0 {
			return false, errors.New("stdin is not interactive")
		}
	}

	reader := bufio.NewReader(in)
	for {
		fmt.Fprintf(out, "Another process is running on port %d, do you want to kill it? [Y/n] ", port)
		answer, err := reader.ReadString('\n')
		if err != nil && strings.TrimSpace(answer) == "" {
			return false, err
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		switch answer {
		case "", "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		default:
			fmt.Fprintln(out, "Please answer yes or no.")
		}
	}
}

func killPortCommands(goos string, port int) [][]string {
	portText := strconv.Itoa(port)
	switch goos {
	case "darwin", "linux", "freebsd", "openbsd", "netbsd":
		return [][]string{
			{"sh", "-c", "pids=$(lsof -ti tcp:" + portText + " 2>/dev/null); [ -z \"$pids\" ] || kill $pids"},
			{"sh", "-c", "fuser -k " + portText + "/tcp 2>/dev/null || true"},
		}
	case "windows":
		script := "$ids=(Get-NetTCPConnection -LocalPort " + portText + " -ErrorAction SilentlyContinue | Select-Object -ExpandProperty OwningProcess -Unique); foreach ($id in $ids) { Stop-Process -Id $id -Force }"
		return [][]string{{"powershell", "-NoProfile", "-Command", script}}
	default:
		return nil
	}
}

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
