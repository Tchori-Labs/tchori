package mcpserv

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// buildTchori compiles the tchori binary into dir and returns its path.
func buildTchori(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "tchori")
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/tchori") //nolint:gosec // fixed command; bin is a t.TempDir artifact
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/tchori: %v\n%s", err, out)
	}
	return bin
}

// rpcResponse is the minimal JSON-RPC 2.0 response envelope the test needs.
type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// readResponse scans newline-delimited JSON from the server's stdout until
// it sees the response whose id equals wantID. Lines without an id
// (server-initiated notifications) and responses to other ids are skipped.
// A JSON-RPC error response for wantID, or EOF, fails the test.
func readResponse(t *testing.T, sc *bufio.Scanner, stderr *bytes.Buffer, wantID int) rpcResponse {
	t.Helper()
	for sc.Scan() {
		line := sc.Bytes()
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("non-JSON line from server: %q (%v)", line, err)
		}
		if len(resp.ID) == 0 || string(resp.ID) == "null" {
			continue // notification, not a response
		}
		var id int
		if err := json.Unmarshal(resp.ID, &id); err != nil {
			continue // non-numeric id (server-initiated request); skip
		}
		if id != wantID {
			continue
		}
		if len(resp.Error) != 0 && string(resp.Error) != "null" {
			t.Fatalf("response id=%d returned JSON-RPC error: %s", wantID, resp.Error)
		}
		return resp
	}
	t.Fatalf("EOF from server before response id=%d (scanner err: %v, stderr: %s)",
		wantID, sc.Err(), stderr.String())
	return rpcResponse{}
}

// TestServeStateListOverStdio starts `tchori mcp` in a workdir seeded with a
// state.json, performs the MCP initialize handshake over stdio pipes (the
// exact wire dialogue captured in research-mcp-sdk.md), and asserts:
//  1. serverInfo.name is "tchori",
//  2. tools/list returns exactly the four contract tools,
//  3. tools/call state_list returns the seeded addresses, sorted, as a
//     single JSON text content block {"addresses":[...]}.
func TestServeStateListOverStdio(t *testing.T) {
	tmp := t.TempDir()
	bin := buildTchori(t, tmp)

	workdir := filepath.Join(tmp, "work")
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		t.Fatal(err)
	}
	stateJSON := `{
  "format_version": "1.0",
  "serial": 3,
  "resources": {
    "null_resource.beta": {"type": "null_resource", "provider": "null", "attributes": {"id": "2"}},
    "null_resource.alpha": {"type": "null_resource", "provider": "null", "attributes": {"id": "1"}}
  }
}` + "\n"
	if err := os.WriteFile(filepath.Join(workdir, "state.json"), []byte(stateJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "mcp") //nolint:gosec // binary built by buildTchori into a temp dir
	cmd.Dir = workdir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close() // EOF => clean server shutdown
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	send := func(msg string) {
		t.Helper()
		if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
			t.Fatalf("write %q: %v (stderr: %s)", msg, err, stderr.String())
		}
	}

	// 1. initialize handshake (exact sequence from research-mcp-sdk.md).
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test-client","version":"0.0.1"}}}`)
	initResp := readResponse(t, sc, &stderr, 1)
	var initResult struct {
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(initResp.Result, &initResult); err != nil {
		t.Fatalf("parse initialize result: %v", err)
	}
	if initResult.ServerInfo.Name != "tchori" {
		t.Fatalf("serverInfo.name = %q, want %q", initResult.ServerInfo.Name, "tchori")
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)

	// 2. tools/list: exactly the four contract tools, no apply tool.
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	listResp := readResponse(t, sc, &stderr, 2)
	var listResult struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listResp.Result, &listResult); err != nil {
		t.Fatalf("parse tools/list result: %v", err)
	}
	got := make([]string, 0, len(listResult.Tools))
	for _, tool := range listResult.Tools {
		got = append(got, tool.Name)
	}
	slices.Sort(got)
	want := []string{"plan", "provider_schema", "state_list", "state_show"}
	if !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}

	// 3. tools/call state_list: seeded addresses, sorted, as JSON text.
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"state_list","arguments":{}}}`)
	callResp := readResponse(t, sc, &stderr, 3)
	var callResult struct {
		IsError bool `json:"isError"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(callResp.Result, &callResult); err != nil {
		t.Fatalf("parse tools/call result: %v", err)
	}
	if callResult.IsError {
		t.Fatalf("state_list returned isError=true: %+v", callResult.Content)
	}
	if len(callResult.Content) != 1 || callResult.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want exactly one text block", callResult.Content)
	}
	var payload struct {
		Addresses []string `json:"addresses"`
	}
	if err := json.Unmarshal([]byte(callResult.Content[0].Text), &payload); err != nil {
		t.Fatalf("state_list text is not JSON: %q (%v)", callResult.Content[0].Text, err)
	}
	wantAddrs := []string{"null_resource.alpha", "null_resource.beta"}
	if !slices.Equal(payload.Addresses, wantAddrs) {
		t.Fatalf("addresses = %v, want %v", payload.Addresses, wantAddrs)
	}
}
