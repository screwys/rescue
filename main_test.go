// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenStoreCreatesDefaultTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rescue.toml")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	data := store.Get()
	if len(data.Instructions) != 2 {
		t.Fatalf("instructions = %d, want 2", len(data.Instructions))
	}
	if len(data.Scripts) != 2 || data.Scripts[0].Filename != "install.sh" || data.Scripts[1].Filename != "script-2.sh" {
		t.Fatalf("unexpected scripts: %#v", data.Scripts)
	}
	if data.Instructions[0].Content != "" || data.Scripts[0].Content != "" {
		t.Fatalf("default blocks should be empty: %#v", data)
	}
}

func TestSaveReloadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rescue.toml")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	next := StoreFile{
		Version:      1,
		Instructions: []Instruction{{ID: "i1", Title: "One", Content: "uname -a"}},
		Scripts:      []Script{{ID: "s1", Filename: "boot.sh", Content: "echo boot"}},
	}
	if err := store.Save(next); err != nil {
		t.Fatal(err)
	}
	if err := store.Reload(); err != nil {
		t.Fatal(err)
	}
	got := store.Get()
	if got.Instructions[0].Content != "uname -a" || got.Scripts[0].Filename != "boot.sh" {
		t.Fatalf("unexpected round trip: %#v", got)
	}
}

func TestValidateRejectsUnsafeScriptFilename(t *testing.T) {
	data := StoreFile{
		Version:      1,
		Instructions: []Instruction{{ID: "i1", Title: "One"}},
		Scripts:      []Script{{ID: "s1", Filename: "../install.sh"}},
	}
	if err := normalizeAndValidate(&data); err == nil {
		t.Fatal("expected unsafe filename error")
	}
}

func TestNormalizeAndValidateRewritesGeneratedIDs(t *testing.T) {
	data := StoreFile{
		Version:      1,
		Instructions: []Instruction{{ID: "instruction-mp23e6c9-riveur", Title: "One"}},
		Scripts:      []Script{{ID: "script-mp23e6c9-riveur", Filename: "install.sh"}},
	}
	if err := normalizeAndValidate(&data); err != nil {
		t.Fatal(err)
	}
	if data.Instructions[0].ID != "instruction-1" {
		t.Fatalf("instruction id = %q", data.Instructions[0].ID)
	}
	if data.Scripts[0].ID != "script-1" {
		t.Fatalf("script id = %q", data.Scripts[0].ID)
	}
}

func TestScriptRouteServesBody(t *testing.T) {
	store := &Store{
		path: "unused",
		data: StoreFile{
			Version:      1,
			Instructions: []Instruction{{ID: "i1", Title: "One"}},
			Scripts:      []Script{{ID: "s1", Filename: "install.sh", Content: "echo ok\n"}},
		},
	}
	app := &App{store: store}
	req := httptest.NewRequest(http.MethodGet, "/install.sh", nil)
	req.SetPathValue("name", "install.sh")
	rec := httptest.NewRecorder()

	app.handleScript(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.String() != "echo ok\n" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

func TestSharedFilesUploadListDownload(t *testing.T) {
	shareDir := t.TempDir()
	app := &App{shareDir: shareDir, hub: NewHub()}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("bring adapter\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/files", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	rec := httptest.NewRecorder()
	app.handleUploadFiles(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %q", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/files", nil)
	rec = httptest.NewRecorder()
	app.handleListFiles(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d", rec.Code)
	}
	var files []SharedFile
	if err := json.Unmarshal(rec.Body.Bytes(), &files); err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "notes.txt" || files[0].Size == 0 {
		t.Fatalf("unexpected files: %#v", files)
	}

	req = httptest.NewRequest(http.MethodGet, "/files/notes.txt", nil)
	req.SetPathValue("name", "notes.txt")
	rec = httptest.NewRecorder()
	app.handleDownloadFile(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("download status = %d", rec.Code)
	}
	if got := rec.Body.String(); got != "bring adapter\n" {
		t.Fatalf("download body = %q", got)
	}
}

func TestSharedFilesZipDownload(t *testing.T) {
	shareDir := t.TempDir()
	app := &App{shareDir: shareDir, hub: NewHub()}
	if err := os.WriteFile(filepath.Join(shareDir, "one.txt"), []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareDir, "two.log"), []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/files.zip?name=two.log", nil)
	rec := httptest.NewRecorder()
	app.handleDownloadZip(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("zip status = %d, body = %q", rec.Code, rec.Body.String())
	}

	reader, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range reader.File {
		src, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(src)
		src.Close()
		if err != nil {
			t.Fatal(err)
		}
		got[file.Name] = string(raw)
	}
	if _, ok := got["one.txt"]; ok {
		t.Fatalf("filtered zip included hidden file: %#v", got)
	}
	if got["two.log"] != "second" {
		t.Fatalf("unexpected zip contents: %#v", got)
	}
}

func TestSharedFilesRejectUnsafeName(t *testing.T) {
	if validSharedFilename("../notes.txt") {
		t.Fatal("expected path traversal name to be rejected")
	}
	if validSharedFilename("bad\nname.txt") {
		t.Fatal("expected control character name to be rejected")
	}
}

func TestResetClearsStateAndSharedFiles(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "rescue.toml"))
	if err != nil {
		t.Fatal(err)
	}
	shareDir := filepath.Join(dir, "rescue-files")
	app := &App{store: store, shareDir: shareDir, hub: NewHub()}

	err = store.Save(StoreFile{
		Version:      1,
		Instructions: []Instruction{{ID: "instruction-1", Title: "Keep", Content: "old note"}},
		Scripts:      []Script{{ID: "script-1", Filename: "run.sh", Content: "echo old"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(shareDir, "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/reset", nil)
	rec := httptest.NewRecorder()
	app.handleReset(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %q", rec.Code, rec.Body.String())
	}

	got := store.Get()
	if got.Instructions[0].Content != "" || got.Scripts[0].Content != "" {
		t.Fatalf("state was not cleared: %#v", got)
	}
	files, err := listSharedFiles(shareDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("shared files were not cleared: %#v", files)
	}
}

func TestStateAPIRejectsDuplicateScriptFilename(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rescue.toml")
	store, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{store: store}
	payload := StoreFile{
		Version:      1,
		Instructions: []Instruction{{ID: "i1", Title: "One"}},
		Scripts: []Script{
			{ID: "s1", Filename: "same.sh"},
			{ID: "s2", Filename: "same.sh"},
		},
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPut, "/api/state", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()

	app.handlePutState(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestKillPortCommands(t *testing.T) {
	if got := killPortCommands("linux", 5000); len(got) == 0 || !strings.Contains(strings.Join(got[0], " "), "5000") {
		t.Fatalf("bad linux commands: %#v", got)
	}
	if got := killPortCommands("windows", 5000); len(got) != 1 || !strings.Contains(strings.Join(got[0], " "), "5000") {
		t.Fatalf("bad windows commands: %#v", got)
	}
	if got := killPortCommands("plan9", 5000); got != nil {
		t.Fatalf("unsupported command = %#v", got)
	}
}

func TestPromptKillBusyPortDefaultsYes(t *testing.T) {
	var out strings.Builder
	yes, err := promptKillBusyPort(5000, strings.NewReader("\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !yes {
		t.Fatal("expected default yes")
	}
	want := "Another process is running on port 5000, do you want to kill it?"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("prompt = %q, want %q", out.String(), want)
	}
}

func TestPromptKillBusyPortNo(t *testing.T) {
	var out strings.Builder
	yes, err := promptKillBusyPort(5000, strings.NewReader("n\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if yes {
		t.Fatal("expected no")
	}
}

func TestOpenBrowserCommands(t *testing.T) {
	url := "http://127.0.0.1:5000"
	if got := openBrowserCommands("windows", url); len(got) != 1 || !strings.Contains(strings.Join(got[0], " "), url) {
		t.Fatalf("bad windows open command: %#v", got)
	}
	if got := openBrowserCommands("darwin", url); len(got) != 1 || got[0][0] != "open" {
		t.Fatalf("bad darwin open command: %#v", got)
	}
	if got := openBrowserCommands("linux", url); len(got) == 0 || got[0][0] != "xdg-open" {
		t.Fatalf("bad linux open command: %#v", got)
	}
	if got := openBrowserCommands("freebsd", url); len(got) == 0 || got[0][0] != "xdg-open" {
		t.Fatalf("bad freebsd open command: %#v", got)
	}
}
