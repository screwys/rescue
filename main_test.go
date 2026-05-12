// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"encoding/json"
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
