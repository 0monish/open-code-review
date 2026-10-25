// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package llm

// Host-agent transport keeps the normal OCR planner/tool loop intact while an
// MCP host (for example, a ChatGPT conversation) supplies individual model
// turns through a separate, explicitly connected local relay. It does NOT
// expose the active ChatGPT model as a callable endpoint on its own.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	hostAgentProtocolVersion = 1
	hostAgentRequestLimit    = 8 << 20
	hostAgentResponseLimit   = 4 << 20
	hostAgentDefaultTimeout  = 5 * time.Minute
)

// HostAgentClient sends one OCR ChatRequest at a time to a loopback-only relay.
// The relay is responsible for making requests available to an MCP client and
// returning that client's explicit response; no ChatGPT credentials are used.
type HostAgentClient struct {
	config ClientConfig
	http   *http.Client
	err    error
}

// NewHostAgentClient creates the dedicated local transport. An invalid relay
// configuration becomes a failed first call, because the LLMClient factory
// intentionally returns an interface (not an interface,error pair).
func NewHostAgentClient(cfg ClientConfig) *HostAgentClient {
	c := &HostAgentClient{config: cfg}
	c.err = validateHostAgentURL(cfg.URL)
	if strings.TrimSpace(cfg.APIKey) == "" {
		c.err = errors.New("host-agent relay requires OCR_HOST_AGENT_TOKEN; no ChatGPT/API credentials are reused")
	}
	// Never use ProxyFromEnvironment: even a local prompt and bearer token must
	// not be redirected to an HTTP proxy or a remote address.
	transport := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 2}
	c.http = &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c
}

func validateHostAgentURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u == nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("host-agent relay URL must be a plain http://127.0.0.1:<port>/v1/complete loopback URL")
	}
	// Literal IP addresses only; allowing localhost would permit DNS rebinding.
	ip := net.ParseIP(u.Hostname())
	if ip == nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") || u.Path != "/v1/complete" {
		return errors.New("host-agent relay requires a numeric loopback address and /v1/complete path")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return errors.New("host-agent relay requires an explicit TCP port between 1 and 65535")
	}
	return nil
}

type hostAgentRequest struct {
	Version   int         `json:"schema_version"`
	Request   ChatRequest `json:"request"`
	SessionID string      `json:"session_id,omitempty"`
}

// The relay cannot supply opaque provider-native reasoning/replay state.
// Only visible text, tool calls and optional token usage are accepted.
type hostAgentResponse struct {
	Version int        `json:"schema_version"`
	ID      string     `json:"id"`
	Model   string     `json:"model"`
	Choices []Choice   `json:"choices"`
	Usage   *UsageInfo `json:"usage,omitempty"`
}

// CompletionsWithCtx fulfills OCR's ordinary LLMClient boundary. OCR still
// decides task planning, high-effort rounds, tool calls, verification, and the
// final report; an MCP host must explicitly service each relay request.
func (c *HostAgentClient) CompletionsWithCtx(ctx context.Context, request ChatRequest) (*ChatResponse, error) {
	if c.err != nil {
		return nil, c.err
	}
	if ctx == nil {
		return nil, errors.New("host-agent completion requires a context")
	}
	if request.Model == "" {
		request.Model = c.config.Model
	}
	if request.Model == "" {
		return nil, errors.New("host-agent completion requires a model label")
	}
	body, err := json.Marshal(hostAgentRequest{
		Version: hostAgentProtocolVersion, Request: request, SessionID: request.SessionID,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal host-agent request: %w", err)
	}
	if len(body) > hostAgentRequestLimit {
		return nil, fmt.Errorf("host-agent request exceeds %d-byte bound", hostAgentRequestLimit)
	}
	timeout := c.config.Timeout
	if timeout <= 0 {
		timeout = hostAgentDefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("construct host-agent request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	httpResp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("host-agent relay did not answer: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode != http.StatusOK {
		// Neither relay-supplied error bodies nor redirect Location headers are
		// echoed: they could contain secrets, untrusted instructions or raw code.
		return nil, fmt.Errorf("host-agent relay returned HTTP %d", httpResp.StatusCode)
	}
	limited := io.LimitReader(httpResp.Body, hostAgentResponseLimit+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read host-agent response: %w", err)
	}
	if len(data) > hostAgentResponseLimit {
		return nil, errors.New("host-agent relay response exceeds size bound")
	}
	var wire hostAgentResponse
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("invalid host-agent response: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, errors.New("host-agent response must contain exactly one JSON document")
	}
	if wire.Version != hostAgentProtocolVersion || len(wire.Choices) != 1 || wire.Choices[0].Message.Role != "assistant" {
		return nil, errors.New("host-agent response has invalid schema version or assistant choice")
	}
	choice := wire.Choices[0]
	if (choice.Message.Content == nil || strings.TrimSpace(*choice.Message.Content) == "") && len(choice.Message.ToolCalls) == 0 {
		return nil, errors.New("host-agent response is empty (no visible text or tool calls)")
	}
	// A relay response has no authority to introduce new OCR tools or claim an
	// authenticated model identity. Enforce the exact tool allowlist from this
	// request, even when a host bypasses OCR Manager and answers the relay
	// directly with a bearer token.
	allowed := make(map[string]struct{}, len(request.Tools))
	for _, def := range request.Tools {
		allowed[def.Function.Name] = struct{}{}
	}
	seenIDs := make(map[string]struct{}, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		if call.ID == "" || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return nil, errors.New("host-agent response contains an invalid tool call")
		}
		if _, ok := allowed[call.Function.Name]; !ok {
			return nil, errors.New("host-agent response requested a tool not offered by OCR")
		}
		if _, duplicate := seenIDs[call.ID]; duplicate {
			return nil, errors.New("host-agent response repeats a tool-call identity")
		}
		seenIDs[call.ID] = struct{}{}
		var args map[string]json.RawMessage
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil || args == nil {
			return nil, errors.New("host-agent tool arguments must be a JSON object")
		}
	}
	return &ChatResponse{
		// The MCP host cannot attest the underlying model; never let it
		// override OCR's explicit, unverified model label in the run report.
		ID: wire.ID, Model: request.Model, Choices: wire.Choices, Usage: wire.Usage,
	}, nil
}
