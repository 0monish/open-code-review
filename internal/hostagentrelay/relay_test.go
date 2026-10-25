// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package hostagentrelay_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/alibaba/open-code-review/internal/hostagentrelay"
	"github.com/alibaba/open-code-review/internal/llm"
)

const token = "test-token-for-the-host-agent-relay-0001"

func newRelayServer(t *testing.T) *httptest.Server {
	t.Helper()
	relay, err := hostagentrelay.New(token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(relay.Handler())
	t.Cleanup(server.Close)
	return server
}

func request(t *testing.T, method, url string, body []byte, key string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, payload
}

func TestHostAgentRelayRoundTripPreservesOCRToolCall(t *testing.T) {
	server := newRelayServer(t)
	client := llm.NewHostAgentClient(llm.ClientConfig{
		URL: server.URL + "/v1/complete", APIKey: token, Model: "mcp-host-agent-unattested",
	})
	type responseResult struct {
		response *llm.ChatResponse
		err      error
	}
	result := make(chan responseResult, 1)
	go func() {
		reply, err := client.CompletionsWithCtx(context.Background(), llm.ChatRequest{
			SessionID: "file-7", Messages: []llm.Message{llm.NewTextMessage("user", "Read file")},
			Tools: []llm.ToolDef{{Type: "function", Function: llm.FunctionDef{Name: "read_file"}}},
		})
		result <- responseResult{reply, err}
	}()
	status, raw := request(t, http.MethodGet, server.URL+"/v1/next?wait_seconds=2", nil, token)
	if status != http.StatusOK {
		t.Fatalf("poll status=%d data=%s", status, raw)
	}
	var turn struct {
		Version   int    `json:"schema_version"`
		TurnID    string `json:"turn_id"`
		SessionID string `json:"session_id"`
		Request   struct {
			Model    string        `json:"model"`
			Messages []llm.Message `json:"messages"`
			Tools    []llm.ToolDef `json:"tools"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &turn); err != nil {
		t.Fatal(err)
	}
	if turn.Version != 1 || len(turn.TurnID) != 32 || turn.SessionID != "file-7" || len(turn.Request.Tools) != 1 || len(turn.Request.Messages) != 1 {
		t.Fatalf("relay lost original OCR request: %#v", turn)
	}
	body := fmt.Sprintf(`{"schema_version":1,"turn_id":%q,"response":{"schema_version":1,"id":"ok","model":"chatgpt-model-unattested","choices":[{"message":{"role":"assistant","content":"read","tool_calls":[{"id":"t1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x.go\"}"}}]},"finish_reason":"tool_calls"}]}}`, turn.TurnID)
	status, raw = request(t, http.MethodPost, server.URL+"/v1/answer", []byte(body), token)
	if status != http.StatusNoContent {
		t.Fatalf("answer status=%d data=%s", status, raw)
	}
	select {
	case done := <-result:
		if done.err != nil {
			t.Fatal(done.err)
		}
		if done.response.Model != "mcp-host-agent-unattested" || done.response.ToolCalls()[0].Function.Name != "read_file" {
			t.Fatalf("wrong completion: %#v", done.response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OCR completion did not finish")
	}
	status, _ = request(t, http.MethodPost, server.URL+"/v1/answer", []byte(body), token)
	if status != http.StatusNotFound {
		t.Fatalf("duplicate submission status=%d, want 404", status)
	}
	status, _ = request(t, http.MethodGet, server.URL+"/v1/next", nil, token)
	if status != http.StatusNoContent {
		t.Fatalf("completed turn was still pending: %d", status)
	}
}

func TestHostAgentRelayRequiresTokenOnEveryEndpoint(t *testing.T) {
	server := newRelayServer(t)
	for _, entry := range []struct{ path, method string }{
		{"/v1/complete", http.MethodPost}, {"/v1/next", http.MethodGet},
		{"/v1/answer", http.MethodPost},
	} {
		for _, key := range []string{"", "wrong"} {
			status, _ := request(t, entry.method, server.URL+entry.path, []byte(`{}`), key)
			if status != http.StatusUnauthorized {
				t.Fatalf("unauthorized %s %s got %d", entry.method, entry.path, status)
			}
		}
	}
}

func TestHostAgentRelayRejectsUnclaimedAnswers(t *testing.T) {
	server := newRelayServer(t)
	status, _ := request(t, http.MethodPost, server.URL+"/v1/answer", []byte(`{"schema_version":1,"turn_id":"0123456789abcdef0123456789abcdef","response":{"schema_version":1}}`), token)
	if status != http.StatusNotFound {
		t.Fatalf("unclaimed answer status=%d", status)
	}
	for _, bad := range []string{`{"schema_version":2}`, `{"schema_version":1,"request":null}`, `bad-json`} {
		status, _ = request(t, http.MethodPost, server.URL+"/v1/complete", []byte(bad), token)
		if status != http.StatusBadRequest {
			t.Fatalf("invalid request accepted: %d", status)
		}
	}
}

func TestHostAgentRelayCancelsAndRejectsStaleResponse(t *testing.T) {
	server := newRelayServer(t)
	client := llm.NewHostAgentClient(llm.ClientConfig{
		URL: server.URL + "/v1/complete", APIKey: token, Model: "mcp-host-agent",
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := client.CompletionsWithCtx(ctx, llm.ChatRequest{}); finished <- err }()
	status, raw := request(t, http.MethodGet, server.URL+"/v1/next?wait_seconds=2", nil, token)
	if status != http.StatusOK {
		t.Fatalf("missing pending request: %d %s", status, raw)
	}
	var turn struct {
		ID string `json:"turn_id"`
	}
	if err := json.Unmarshal(raw, &turn); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("cancelled completion succeeded")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OCR did not cancel")
	}
	body := fmt.Sprintf(`{"schema_version":1,"turn_id":%q,"response":{"schema_version":1,"choices":[]}}`, turn.ID)
	for attempt := 0; attempt < 15; attempt++ {
		status, _ = request(t, http.MethodPost, server.URL+"/v1/answer", []byte(body), token)
		if status == http.StatusNotFound {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("canceled turn still allowed a reply: %d", status)
}

func TestHostAgentRelayConcurrentTurnsAreMatchedByIdentity(t *testing.T) {
	server := newRelayServer(t)
	client := llm.NewHostAgentClient(llm.ClientConfig{URL: server.URL + "/v1/complete", APIKey: token, Model: "host"})
	const count = 4
	results := make(chan string, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := client.CompletionsWithCtx(context.Background(), llm.ChatRequest{SessionID: fmt.Sprint(i), Messages: []llm.Message{llm.NewTextMessage("user", fmt.Sprintf("task-%d", i))}})
			if err != nil {
				results <- err.Error()
				return
			}
			results <- resp.Content()
		}(i)
	}
	for i := 0; i < count; i++ {
		status, raw := request(t, http.MethodGet, server.URL+"/v1/next?wait_seconds=2", nil, token)
		if status != 200 {
			t.Fatalf("missed concurrent request: %s", raw)
		}
		var turn struct {
			ID        string `json:"turn_id"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(raw, &turn); err != nil {
			t.Fatal(err)
		}
		body := fmt.Sprintf(`{"schema_version":1,"turn_id":%q,"response":{"schema_version":1,"choices":[{"message":{"role":"assistant","content":%q}}]}}`, turn.ID, turn.SessionID)
		status, _ = request(t, http.MethodPost, server.URL+"/v1/answer", []byte(body), token)
		if status != 204 {
			t.Fatalf("concurrent answer status=%d", status)
		}
	}
	wg.Wait()
	close(results)
	seen := map[string]bool{}
	for result := range results {
		if seen[result] {
			t.Fatalf("duplicate result %q", result)
		}
		seen[result] = true
	}
	for i := 0; i < count; i++ {
		if !seen[fmt.Sprint(i)] {
			t.Fatalf("task %d did not get its own answer: %#v", i, seen)
		}
	}
}
