// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const relayTestToken = "local-relay-token-only-for-unit-tests"

func newHostAgentTestClient(url string) LLMClient {
	return NewLLMClient(ResolvedEndpoint{
		Protocol: ProtocolHostAgent,
		URL:      url,
		Token:    relayTestToken,
		Model:    "current-mcp-client-unattested",
	}, nil, nil)
}

func TestHostAgentModelTurnPreservesOCRToolsAndUsage(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/complete" {
			t.Errorf("request method/path = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+relayTestToken {
			t.Errorf("auth header missing or wrong")
		}
		var request hostAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode relay request: %v", err)
		}
		if request.Version != 1 || request.SessionID != "review-task-5" {
			t.Errorf("relay envelope does not preserve session identity: %#v", request)
		}
		if len(request.Request.Messages) != 2 || len(request.Request.Tools) != 1 {
			t.Errorf("OCR planning/tool state was lost: %#v", request)
		}
		if request.Request.Model != "current-mcp-client-unattested" {
			t.Errorf("model label = %q", request.Request.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"schema_version":1,"id":"turn-5","model":"relay-unspecified","choices":[{"message":{"role":"assistant","content":"Need the file","tool_calls":[{"id":"tool-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	}))
	defer server.Close()
	client := newHostAgentTestClient(server.URL + "/v1/complete")
	_, ok := client.(*HostAgentClient)
	if !ok {
		t.Fatalf("protocol dispatch = %T, want HostAgentClient", client)
	}
	response, err := client.CompletionsWithCtx(context.Background(), ChatRequest{
		Messages:  []Message{NewTextMessage("system", "Review carefully"), NewTextMessage("user", "Find bugs")},
		SessionID: "review-task-5",
		Tools:     []ToolDef{{Type: "function", Function: FunctionDef{Name: "read_file", Description: "Read file"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 || response.Content() != "Need the file" || len(response.ToolCalls()) != 1 {
		t.Fatalf("response = %#v, calls = %d", response, callCount)
	}
	if response.ToolCalls()[0].Function.Name != "read_file" || response.ToolCalls()[0].Function.Arguments != `{"path":"x.go"}` {
		t.Fatalf("tool call corrupted: %#v", response.ToolCalls()[0])
	}
	if response.Model != "current-mcp-client-unattested" {
		t.Fatalf("host claimed unverified model identity: %q", response.Model)
	}
	if response.Usage == nil || response.Usage.TotalTokens != 15 {
		t.Fatalf("relay usage did not roundtrip: %#v", response.Usage)
	}
}

func TestHostAgentURLFailsClosedBeforeNetwork(t *testing.T) {
	for _, value := range []string{
		"", "https://127.0.0.1:8123/v1/complete", "http://localhost:8123/v1/complete",
		"http://example.org:8123/v1/complete", "http://127.0.0.2:8123/v1/complete",
		"http://127.0.0.1:8123/other", "http://127.0.0.1/v1/complete",
		"http://user:pass@127.0.0.1:8123/v1/complete",
		"http://127.0.0.1:8123/v1/complete?x=1",
		"http://127.0.0.1:8123/v1/complete#fragment",
	} {
		t.Run(value, func(t *testing.T) {
			client := newHostAgentTestClient(value)
			if _, err := client.CompletionsWithCtx(context.Background(), ChatRequest{Model: "x"}); err == nil {
				t.Fatalf("unsafe bridge URL %q accepted", value)
			}
		})
	}
	if err := validateHostAgentURL("http://[::1]:8123/v1/complete"); err != nil {
		t.Fatalf("IPv6 loopback should be allowed: %v", err)
	}
}

func TestHostAgentRedirectDoesNotLeakBearer(t *testing.T) {
	var leaked atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
	}))
	defer remote.Close()
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, remote.URL, http.StatusTemporaryRedirect)
	}))
	defer local.Close()
	_, err := newHostAgentTestClient(local.URL+"/v1/complete").CompletionsWithCtx(context.Background(), ChatRequest{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "HTTP 307") || leaked.Load() {
		t.Fatalf("redirect must fail without exposing token; err=%v leak=%v", err, leaked.Load())
	}
}

func TestHostAgentRejectsInvalidResponses(t *testing.T) {
	for _, data := range []string{
		`not-json`,
		`{"schema_version":9,"choices":[]}`,
		`{"schema_version":1,"choices":[{"message":{"role":"user","content":"wrong"}}]}`,
		`{"schema_version":1,"choices":[{"message":{"role":"assistant","content":""}}]}`,
		`{"schema_version":1,"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"tool-1","function":{"name":"read_file","arguments":"oops"}}]}}]}`,
		`{"schema_version":1,"choices":[{"message":{"role":"assistant","content":"ok"}}]}{"bad":true}`,
	} {
		t.Run(data, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, data)
			}))
			defer server.Close()
			if _, err := newHostAgentTestClient(server.URL+"/v1/complete").CompletionsWithCtx(context.Background(), ChatRequest{Model: "m"}); err == nil {
				t.Fatal("malformed host-agent model response accepted")
			}
		})
	}
}

func TestHostAgentNeverExecutesUnofferedOrDuplicateToolCalls(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
	}{
		{"unoffered", `{"schema_version":1,"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"tool-1","function":{"name":"delete_everything","arguments":"{}"}}]}}]}`},
		{"duplicate tool IDs", `{"schema_version":1,"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"tool-1","function":{"name":"read_file","arguments":"{}"}},{"id":"tool-1","function":{"name":"read_file","arguments":"{}"}}]}}]}`},
		{"non-object tool args", `{"schema_version":1,"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"tool-1","function":{"name":"read_file","arguments":"null"}}]}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, tc.reply)
			}))
			defer server.Close()
			_, err := newHostAgentTestClient(server.URL+"/v1/complete").CompletionsWithCtx(
				context.Background(), ChatRequest{
					Model: "unattested", Tools: []ToolDef{{Type: "function", Function: FunctionDef{Name: "read_file"}}},
				},
			)
			if err == nil {
				t.Fatalf("unsafe host model request accepted: %s", tc.reply)
			}
		})
	}
}

func TestHostAgentCancelledRequestDoesNotWaitForModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Some Go HTTP server configurations do not observe client cancellation
		// until the handler returns. Never let the *test server* block Close.
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := newHostAgentTestClient(server.URL+"/v1/complete").CompletionsWithCtx(ctx, ChatRequest{Model: "m"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation not respected: %v", err)
	}
}

func TestExplicitHostAgentBypassesOtherSignedOutProvider(t *testing.T) {
	t.Setenv("OCR_HOST_AGENT_URL", "http://127.0.0.1:8373/v1/complete")
	t.Setenv("OCR_HOST_AGENT_TOKEN", relayTestToken)
	t.Setenv("OCR_HOST_AGENT_MODEL", "client-selected")
	t.Setenv("OCR_LLM_TIMEOUT", "900")
	// A deliberately invalid provider config still cannot shadow an explicit
	// --provider host-agent, which is essential when ChatGPT auth is signed out.
	config := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(config, []byte(`{not-json}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ep, err := ResolveEndpointWithOptions(config, ResolveOptions{Provider: "host-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if ep.Protocol != ProtocolHostAgent || ep.Model != "client-selected" || ep.Timeout != 900*time.Second {
		t.Fatalf("host-agent endpoint not resolved: %#v", ep)
	}
	t.Setenv("OCR_HOST_AGENT_TOKEN", "")
	if _, err := ResolveEndpointWithOptions(config, ResolveOptions{Provider: "host-agent"}); err == nil {
		t.Fatal("missing relay secret should fail closed")
	}
}
