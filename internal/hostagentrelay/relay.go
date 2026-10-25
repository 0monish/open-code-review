// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

// Package hostagentrelay holds an authenticated, bounded, ephemeral queue of
// OCR model turns. An external MCP host must poll and answer each turn; the
// relay does not and cannot call the active ChatGPT conversation itself.
package hostagentrelay

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	protocolVersion = 1
	maxRequestBody  = 8 << 20
	maxAnswerBody   = 4 << 20
	maxInFlight     = 16
	leaseDuration   = 30 * time.Second
)

type turn struct {
	id        string
	request   json.RawMessage
	sessionID string
	claimedAt time.Time
	response  chan []byte
}

type pendingInput struct {
	Version   int             `json:"schema_version"`
	Request   json.RawMessage `json:"request"`
	SessionID string          `json:"session_id,omitempty"`
}

type claimedTurn struct {
	Version   int             `json:"schema_version"`
	TurnID    string          `json:"turn_id"`
	Request   json.RawMessage `json:"request"`
	SessionID string          `json:"session_id,omitempty"`
}

type answerInput struct {
	Version  int             `json:"schema_version"`
	TurnID   string          `json:"turn_id"`
	Response json.RawMessage `json:"response"`
}

// Relay is intentionally in-memory, no model secrets are persisted. It is
// safe to construct for one short-lived local review worker. Restarting the
// relay aborts pending model calls instead of replaying stale answers.
type Relay struct {
	mu    sync.Mutex
	token string
	turns map[string]*turn
	order []string
	wake  chan struct{}
}

// New validates the authorization token. Callers must generate/store it with
// filesystem-appropriate protections; it is never returned by status APIs.
func New(token string) (*Relay, error) {
	if len(token) < 24 {
		return nil, fmt.Errorf("host-agent relay needs a random 24+ character token")
	}
	return &Relay{token: token, turns: make(map[string]*turn), wake: make(chan struct{}, 1)}, nil
}

func (r *Relay) authorize(w http.ResponseWriter, req *http.Request) bool {
	expected := []byte("Bearer " + r.token)
	supplied := []byte(req.Header.Get("Authorization"))
	if subtle.ConstantTimeCompare(supplied, expected) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

func readJSON(req *http.Request, limit int64, value any) error {
	body, err := io.ReadAll(io.LimitReader(req.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(body)) > limit {
		return fmt.Errorf("payload too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("multiple JSON documents")
	}
	return nil
}

func (r *Relay) removeLocked(id string) {
	delete(r.turns, id)
	for i, candidate := range r.order {
		if candidate == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

func (r *Relay) enqueue(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body pendingInput
	if err := readJSON(req, maxRequestBody, &body); err != nil || body.Version != protocolVersion || len(body.Request) == 0 || !json.Valid(body.Request) || !bytes.HasPrefix(bytes.TrimSpace(body.Request), []byte("{")) {
		http.Error(w, "invalid model request", http.StatusBadRequest)
		return
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		http.Error(w, "cannot allocate request", http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(nonce[:])
	item := &turn{id: id, request: append(json.RawMessage(nil), body.Request...), sessionID: body.SessionID, response: make(chan []byte, 1)}
	r.mu.Lock()
	if len(r.turns) >= maxInFlight {
		r.mu.Unlock()
		http.Error(w, "relay is at capacity", http.StatusServiceUnavailable)
		return
	}
	r.turns[id] = item
	r.order = append(r.order, id)
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	defer func() { r.mu.Lock(); r.removeLocked(id); r.mu.Unlock() }()
	select {
	case answer := <-item.response:
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(answer)
	case <-req.Context().Done():
		// A canceled request can never consume a later host answer.
	}
}

func (r *Relay) nextLocked() *turn {
	now := time.Now()
	for _, id := range r.order {
		candidate := r.turns[id]
		if candidate != nil && (candidate.claimedAt.IsZero() || now.Sub(candidate.claimedAt) >= leaseDuration) {
			candidate.claimedAt = now
			return candidate
		}
	}
	return nil
}

func (r *Relay) next(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	seconds := 0
	if raw := req.URL.Query().Get("wait_seconds"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > 20 {
			http.Error(w, "wait_seconds must be 0..20", http.StatusBadRequest)
			return
		}
		seconds = parsed
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for {
		r.mu.Lock()
		item := r.nextLocked()
		r.mu.Unlock()
		if item != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(claimedTurn{
				Version: protocolVersion, TurnID: item.id, Request: item.request, SessionID: item.sessionID,
			})
			return
		}
		if seconds == 0 || !time.Now().Before(deadline) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		remaining := time.Until(deadline)
		if remaining > time.Second {
			remaining = time.Second
		}
		timer := time.NewTimer(remaining)
		select {
		case <-r.wake:
		case <-timer.C:
		case <-req.Context().Done():
			timer.Stop()
			return
		}
		timer.Stop()
	}
}

func (r *Relay) answer(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body answerInput
	if err := readJSON(req, maxAnswerBody, &body); err != nil || body.Version != protocolVersion || len(body.TurnID) != 32 || !json.Valid(body.Response) || len(body.Response) == 0 {
		http.Error(w, "invalid relay answer", http.StatusBadRequest)
		return
	}
	var responseHeader struct {
		Version int `json:"schema_version"`
	}
	if err := json.Unmarshal(body.Response, &responseHeader); err != nil || responseHeader.Version != protocolVersion {
		http.Error(w, "invalid answer version", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	item, exists := r.turns[body.TurnID]
	if !exists || item.claimedAt.IsZero() {
		r.mu.Unlock()
		http.Error(w, "model request missing or unclaimed", http.StatusNotFound)
		return
	}
	r.removeLocked(body.TurnID)
	r.mu.Unlock()
	item.response <- append([]byte(nil), body.Response...)
	w.WriteHeader(http.StatusNoContent)
}

// Handler serves three small endpoints. The caller MUST bind the http.Server
// to a numeric loopback interface and keep the bearer token private. No
// permissive CORS or browser-cookie authorization is configured.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/complete", func(w http.ResponseWriter, req *http.Request) {
		if r.authorize(w, req) {
			r.enqueue(w, req)
		}
	})
	mux.HandleFunc("/v1/next", func(w http.ResponseWriter, req *http.Request) {
		if r.authorize(w, req) {
			r.next(w, req)
		}
	})
	mux.HandleFunc("/v1/answer", func(w http.ResponseWriter, req *http.Request) {
		if r.authorize(w, req) {
			r.answer(w, req)
		}
	})
	return mux
}

// Requests are never resumed after relay restart. This is deliberate: a
// ChatGPT client turn should be answered once, against its exact OCR request.
