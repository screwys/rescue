// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type RunLogInfo struct {
	ID       string     `json:"id"`
	Name     string     `json:"name"`
	Device   string     `json:"device"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
	ExitCode *int       `json:"exitCode,omitempty"`
	Size     int64      `json:"size"`
}

type runLogRecord struct {
	RunLogInfo
	Token string `json:"token"`
}

type runLog struct {
	mu     sync.Mutex
	upload sync.Mutex
	record runLogRecord
}

type RunLogs struct {
	mu   sync.RWMutex
	dir  string
	runs map[string]*runLog
}

func OpenRunLogs(storePath string) (*RunLogs, error) {
	dir := filepath.Join(filepath.Dir(storePath), "rescue-logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	logs := &RunLogs{dir: dir, runs: make(map[string]*runLog)}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var record runLogRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("run log %s: %w", entry.Name(), err)
		}
		if record.ID != strings.TrimSuffix(entry.Name(), ".json") {
			return nil, fmt.Errorf("run log %s: ID does not match filename", entry.Name())
		}
		for _, privatePath := range []string{path, logs.outputPath(record.ID)} {
			if err := os.Chmod(privatePath, 0o600); err != nil {
				return nil, err
			}
			if err := protectPrivateFile(privatePath); err != nil {
				return nil, err
			}
		}
		logs.runs[record.ID] = &runLog{record: record}
	}
	return logs, nil
}

func (l *RunLogs) outputPath(id string) string {
	return filepath.Join(l.dir, id+".log")
}

func (l *RunLogs) Create(name, device string) (id, token string, err error) {
	id, token = randomID(), randomID()
	record := runLogRecord{RunLogInfo: RunLogInfo{ID: id, Name: name, Device: device, Started: time.Now().UTC()}, Token: token}
	path := l.outputPath(id)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", err
	}
	if err = f.Close(); err == nil {
		err = protectPrivateFile(path)
	}
	if err == nil {
		err = writePrivateJSON(filepath.Join(l.dir, id+".json"), record)
	}
	if err != nil {
		os.Remove(path)
		return "", "", err
	}
	l.mu.Lock()
	l.runs[id] = &runLog{record: record}
	l.mu.Unlock()
	return id, token, nil
}

func (l *RunLogs) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/logs", l.list)
	mux.HandleFunc("POST /api/logs", l.create)
	mux.HandleFunc("POST /api/logs/{id}/output", l.appendOutput)
	mux.HandleFunc("POST /api/logs/{id}/finish", l.finish)
	mux.HandleFunc("GET /api/logs/{id}/output", l.readOutput)
	mux.HandleFunc("GET /api/logs/{id}/download", l.download)
}

func (l *RunLogs) lookup(w http.ResponseWriter, r *http.Request, authorize bool) *runLog {
	l.mu.RLock()
	run := l.runs[r.PathValue("id")]
	l.mu.RUnlock()
	if run == nil {
		http.NotFound(w, r)
		return nil
	}
	run.mu.Lock()
	token := run.record.Token
	run.mu.Unlock()
	if authorize && subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		http.Error(w, "upload token rejected", http.StatusForbidden)
		return nil
	}
	return run
}

func (l *RunLogs) create(w http.ResponseWriter, r *http.Request) {
	if !sameOriginMutation(w, r) {
		return
	}
	var options struct {
		Name   string `json:"name"`
		Device string `json:"device"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&options); err != nil {
		http.Error(w, "invalid run log", http.StatusBadRequest)
		return
	}
	id, token, err := l.Create(options.Name, options.Device)
	if err != nil {
		http.Error(w, "could not create log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, map[string]string{"id": id, "token": token})
}

func (l *RunLogs) list(w http.ResponseWriter, r *http.Request) {
	l.mu.RLock()
	runs := make([]*runLog, 0, len(l.runs))
	for _, run := range l.runs {
		runs = append(runs, run)
	}
	l.mu.RUnlock()
	infos := make([]RunLogInfo, 0, len(runs))
	for _, run := range runs {
		run.mu.Lock()
		info := run.record.RunLogInfo
		stat, err := os.Stat(l.outputPath(info.ID))
		if err == nil {
			info.Size = stat.Size()
		}
		run.mu.Unlock()
		if err != nil {
			http.Error(w, "could not read log: "+err.Error(), http.StatusInternalServerError)
			return
		}
		infos = append(infos, info)
	}
	slices.SortFunc(infos, func(a, b RunLogInfo) int { return b.Started.Compare(a.Started) })
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, infos)
}

func (l *RunLogs) appendOutput(w http.ResponseWriter, r *http.Request) {
	run := l.lookup(w, r, true)
	if run == nil {
		return
	}
	run.upload.Lock()
	defer run.upload.Unlock()
	f, err := os.OpenFile(l.outputPath(run.record.ID), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		http.Error(w, "could not open log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := r.Body.Read(buffer)
		if n > 0 {
			run.mu.Lock()
			_, err = f.Write(buffer[:n])
			run.mu.Unlock()
			if err != nil {
				http.Error(w, "could not write log: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			http.Error(w, "could not read output: "+readErr.Error(), http.StatusBadRequest)
			return
		}
	}
	if err := f.Close(); err != nil {
		http.Error(w, "could not close log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (l *RunLogs) finish(w http.ResponseWriter, r *http.Request) {
	run := l.lookup(w, r, true)
	if run == nil {
		return
	}
	var result struct {
		ExitCode *int `json:"exitCode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&result); err != nil || result.ExitCode == nil {
		http.Error(w, "invalid run status", http.StatusBadRequest)
		return
	}
	run.upload.Lock()
	defer run.upload.Unlock()
	run.mu.Lock()
	defer run.mu.Unlock()
	next := run.record
	now := time.Now().UTC()
	next.Finished, next.ExitCode = &now, result.ExitCode
	if err := writePrivateJSON(filepath.Join(l.dir, next.ID+".json"), next); err != nil {
		http.Error(w, "could not save run status: "+err.Error(), http.StatusInternalServerError)
		return
	}
	run.record = next
	w.WriteHeader(http.StatusNoContent)
}

func (l *RunLogs) readOutput(w http.ResponseWriter, r *http.Request) {
	run := l.lookup(w, r, false)
	if run == nil {
		return
	}
	var offset int64
	if value := r.URL.Query().Get("offset"); value != "" {
		var err error
		offset, err = strconv.ParseInt(value, 10, 64)
		if err != nil || offset < 0 {
			http.Error(w, "invalid log offset", http.StatusBadRequest)
			return
		}
	}
	run.mu.Lock()
	f, err := os.Open(l.outputPath(run.record.ID))
	var data []byte
	finished := run.record.Finished != nil
	if err == nil {
		_, err = f.Seek(offset, io.SeekStart)
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(f, 256<<10))
		}
		f.Close()
	}
	run.mu.Unlock()
	if err != nil {
		http.Error(w, "could not read log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Keep an unfinished UTF-8 character for the next poll.
	end := len(data)
	if end > 0 {
		start := end - 1
		for start > 0 && !utf8.RuneStart(data[start]) {
			start--
		}
		if !utf8.FullRune(data[start:]) && (!finished || len(data) == 256<<10) {
			end = start
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Log-Offset", strconv.FormatInt(offset+int64(end), 10))
	io.WriteString(w, strings.ToValidUTF8(string(data[:end]), "\ufffd"))
}

func (l *RunLogs) download(w http.ResponseWriter, r *http.Request) {
	run := l.lookup(w, r, false)
	if run == nil {
		return
	}
	run.mu.Lock()
	f, err := os.Open(l.outputPath(run.record.ID))
	var stat os.FileInfo
	if err == nil {
		stat, err = f.Stat()
	}
	name := run.record.Name
	run.mu.Unlock()
	if f != nil {
		defer f.Close()
	}
	if err != nil {
		http.Error(w, "could not read log: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".log"}))
	http.ServeContent(w, r, name+".log", stat.ModTime(), io.NewSectionReader(f, 0, stat.Size()))
}
