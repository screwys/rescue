// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var commandStdout io.Writer = os.Stdout
var commandStderr io.Writer = os.Stderr
var commandReport *runReporter

// Keep network writes out of the program's stdout and stderr paths.
type runReporter struct {
	mu                              sync.Mutex
	file                            *os.File
	server, name, device, id, token string
	offset                          int64
	client                          *http.Client
	stop                            chan struct{}
	done                            chan struct{}
	writeErr                        error
}

func startRunReport(server, name, id, token string) {
	if server == "" || commandReport != nil {
		return
	}
	file, err := os.CreateTemp("", "rescue-output-*.log")
	if err == nil {
		err = file.Chmod(0o600)
	}
	if err == nil {
		err = protectPrivateFile(file.Name())
	}
	if err != nil {
		if file != nil {
			file.Close()
			os.Remove(file.Name())
		}
		fmt.Fprintln(os.Stderr, "rescue: log file:", err)
		return
	}
	device, _ := os.Hostname()
	report := &runReporter{file: file, server: strings.TrimRight(server, "/"), name: name, device: device, id: id, token: token, client: &http.Client{Timeout: 2 * time.Second}, stop: make(chan struct{}), done: make(chan struct{})}
	commandReport = report
	commandStdout = io.MultiWriter(os.Stdout, report)
	commandStderr = io.MultiWriter(os.Stderr, report)
	log.SetOutput(commandStderr)
	go report.uploadLoop()
}

func (report *runReporter) Write(data []byte) (int, error) {
	report.mu.Lock()
	defer report.mu.Unlock()
	if report.writeErr == nil {
		_, report.writeErr = report.file.Write(data)
	}
	return len(data), nil
}

func (report *runReporter) request(method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, report.server+path, body)
	if err != nil {
		return nil, err
	}
	if report.token != "" {
		req.Header.Set("Authorization", "Bearer "+report.token)
	}
	req.Header.Set("Content-Type", contentType)
	return report.client.Do(req)
}

func (report *runReporter) upload() error {
	if report.id == "" {
		data, _ := json.Marshal(map[string]string{"name": report.name, "device": report.device})
		response, err := report.request(http.MethodPost, "/api/logs", "application/json", bytes.NewReader(data))
		if err != nil {
			return err
		}
		var result struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		}
		if response.StatusCode != http.StatusCreated {
			response.Body.Close()
			return fmt.Errorf("create log: %s", response.Status)
		}
		err = json.NewDecoder(response.Body).Decode(&result)
		response.Body.Close()
		if err != nil {
			return err
		}
		report.mu.Lock()
		report.id, report.token = result.ID, result.Token
		report.mu.Unlock()
	}
	for {
		data := make([]byte, 64<<10)
		report.mu.Lock()
		n, err := report.file.ReadAt(data, report.offset)
		report.mu.Unlock()
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			return nil
		}
		response, err := report.request(http.MethodPost, "/api/logs/"+report.id+"/output", "text/plain; charset=utf-8", bytes.NewReader(data[:n]))
		if err != nil {
			return err
		}
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode >= 300 {
			return fmt.Errorf("upload log: %s", response.Status)
		}
		report.offset += int64(n)
		report.mu.Lock()
		if info, err := report.file.Stat(); err == nil && info.Size() == report.offset {
			if err := report.file.Truncate(0); err == nil {
				if _, err := report.file.Seek(0, io.SeekStart); err == nil {
					report.offset = 0
				}
			}
		}
		report.mu.Unlock()
	}
}

func (report *runReporter) credentials() (string, string) {
	report.mu.Lock()
	defer report.mu.Unlock()
	return report.id, report.token
}

func (report *runReporter) uploadLoop() {
	defer close(report.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-report.stop:
			return
		case <-ticker.C:
			_ = report.upload()
		}
	}
}

func (report *runReporter) Finish(code int) {
	close(report.stop)
	<-report.done
	err := report.upload()
	if err == nil {
		report.mu.Lock()
		err = report.writeErr
		report.mu.Unlock()
	}
	if err == nil {
		data, _ := json.Marshal(map[string]int{"exitCode": code})
		response, finishErr := report.request(http.MethodPost, "/api/logs/"+report.id+"/finish", "application/json", bytes.NewReader(data))
		if finishErr != nil {
			err = finishErr
		} else {
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode >= 300 {
				err = fmt.Errorf("finish log: %s", response.Status)
			}
		}
	}
	report.file.Close()
	if err == nil {
		os.Remove(report.file.Name())
	} else {
		fmt.Fprintf(os.Stderr, "rescue: log upload: %v; output saved in %s\n", err, report.file.Name())
	}
}
