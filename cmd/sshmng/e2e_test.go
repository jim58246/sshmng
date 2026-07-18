package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// jsonRPCRequest 是 JSON-RPC 2.0 请求的最小结构。
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// startBinary 启动 sshmng 二进制，连接到临时 config 路径，返回 stdin 写入器和 stdout 读取器。
func startBinary(t *testing.T, configPath string) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	bin := os.Getenv("SSHMNG_BIN")
	if bin == "" {
		bin = "/tmp/sshmng"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("binary not found at %s: %v (build with `go build -o /tmp/sshmng ./cmd/sshmng`)", bin, err)
	}
	cmd := exec.Command(bin, "--config", configPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	return cmd, stdin, bufio.NewReader(stdout)
}

// sendReq 把 JSON-RPC 请求写入 stdin。
func sendReq(t *testing.T, w io.Writer, req jsonRPCRequest) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	data = append(data, '\n')
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readResp 从 stdout 读取一行 JSON-RPC 响应。超时 5s。
func readResp(t *testing.T, r *bufio.Reader) map[string]any {
	t.Helper()
	type result struct {
		data []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadBytes('\n')
		ch <- result{line, err}
	}()
	select {
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for response")
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read: %v", res.err)
		}
		var m map[string]any
		if err := json.Unmarshal(res.data, &m); err != nil {
			t.Fatalf("parse %q: %v", string(res.data), err)
		}
		return m
	}
	return nil
}

// readRespFor 读取响应直到匹配 id。跳过 notification（无 id 字段）。
func readRespFor(t *testing.T, r *bufio.Reader, id int) map[string]any {
	t.Helper()
	for {
		m := readResp(t, r)
		if rawID, ok := m["id"]; ok {
			if int(rawID.(float64)) == id {
				return m
			}
		}
		// 否则是 notification，跳过继续读
	}
}

func TestE2EBinaryInitializeAndListTools(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	_, stdin, stdout := startBinary(t, configPath)

	// 1. initialize
	sendReq(t, stdin, jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e-test", "version": "1"},
		},
	})
	initResp := readRespFor(t, stdout, 1)
	if _, ok := initResp["result"]; !ok {
		t.Fatalf("init response missing result: %v", initResp)
	}

	// 2. notifications/initialized (no response expected)
	sendReq(t, stdin, jsonRPCRequest{
		JSONRPC: "2.0", Method: "notifications/initialized",
	})

	// 3. tools/list
	sendReq(t, stdin, jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list"})
	listResp := readRespFor(t, stdout, 2)
	result, ok := listResp["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list response missing result: %v", listResp)
	}
	tools := result["tools"].([]any)
	want := map[string]bool{
		"list_ssh_servers": false, "get_ssh_server": false, "update_ssh_server": false,
		"list_jumphosts": false, "get_jumphost": false, "update_jumphost": false,
		"list_proxies": false, "get_proxy": false, "update_proxy": false,
	}
	for _, tAny := range tools {
		tool := tAny.(map[string]any)
		name := tool["name"].(string)
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q not registered", name)
		}
	}
}

func TestE2ECreateAndListSSHServer(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	_, stdin, stdout := startBinary(t, configPath)

	// initialize + initialized notification
	sendReq(t, stdin, jsonRPCRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "e2e-test", "version": "1"},
		},
	})
	readRespFor(t, stdout, 1)
	sendReq(t, stdin, jsonRPCRequest{JSONRPC: "2.0", Method: "notifications/initialized"})

	// update_ssh_server: create "s1"
	sendReq(t, stdin, jsonRPCRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/call",
		Params: map[string]any{
			"name": "update_ssh_server",
			"arguments": map[string]any{
				"name": "s1",
				"patch": map[string]any{
					"addr": "1.1.1.1:22",
					"user": "u",
					"auth": map[string]any{"password": "secret"},
				},
			},
		},
	})
	createResp := readRespFor(t, stdout, 2)
	if _, ok := createResp["result"]; !ok {
		t.Fatalf("update response missing result: %v", createResp)
	}

	// list_ssh_servers
	sendReq(t, stdin, jsonRPCRequest{
		JSONRPC: "2.0", ID: 3, Method: "tools/call",
		Params: map[string]any{"name": "list_ssh_servers", "arguments": map[string]any{}},
	})
	listResp := readRespFor(t, stdout, 3)
	result := listResp["result"].(map[string]any)
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "s1") {
		t.Errorf("list result should contain 's1': %s", text)
	}
	if strings.Contains(text, "secret") {
		t.Errorf("list result must not contain password 'secret': %s", text)
	}

	// Verify file persisted with 0600
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file perm = %o, want 0600", perm)
	}

	// 读取文件，验证 password 已持久化（list 不显示但 store 里有）
	data, _ := os.ReadFile(configPath)
	if !strings.Contains(string(data), "secret") {
		t.Errorf("config file should contain password 'secret': %s", string(data))
	}
}
