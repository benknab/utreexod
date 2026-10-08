// Copyright (c) 2026 The utreexo developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/utreexo/utreexod/btcjson"
)

const (
	publicRPCMaxBody        = 4096
	publicRPCMaxBatch       = 8
	publicRPCMaxReply       = 256 * 1024
	publicRPCMaxConnections = 32
	publicRPCMaxRequests    = 4
	publicRPCRate           = 20 // Calls per second, with a burst of 20.
	publicRPCTimeout        = 30 * time.Second
)

// rpcPublic is independent of rpcLimited. Credentials never grant additional
// permissions on the public listener. New native methods are private by default.
var rpcPublic = map[string]struct{}{
	"getbestblockhash":           {},
	"getblockhash":               {},
	"getblockheader":             {},
	"gettxout":                   {},
	"getutreexoroots":            {},
	"proveutxochaintipinclusion": {},
}

type publicRPCHandler struct {
	dispatch func(*parsedRPCCmd, <-chan struct{}) (interface{}, error)
	requests chan struct{}
	proofs   chan struct{}
	rateMu   sync.Mutex
	tokens   float64
	last     time.Time
}

func newPublicRPCServer(dispatch func(*parsedRPCCmd, <-chan struct{}) (interface{}, error)) *http.Server {
	handler := &publicRPCHandler{
		dispatch: dispatch,
		requests: make(chan struct{}, publicRPCMaxRequests),
		proofs:   make(chan struct{}, 1),
		tokens:   publicRPCRate,
		last:     time.Now(),
	}
	var connections int32
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      publicRPCTimeout,
		IdleTimeout:       10 * time.Second,
		MaxHeaderBytes:    4096,
		ConnState: func(conn net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				if atomic.AddInt32(&connections, 1) > publicRPCMaxConnections {
					conn.Close()
				}
			case http.StateClosed, http.StateHijacked:
				atomic.AddInt32(&connections, -1)
			}
		},
	}
}

func (s *rpcServer) startPublicRPC() {
	if s.publicHTTPServer == nil {
		return
	}
	for _, listener := range s.cfg.PublicListeners {
		s.wg.Add(1)
		go func(listener net.Listener) {
			defer s.wg.Done()
			rpcsLog.Infof("Public RPC server listening on %s", listener.Addr())
			if err := s.publicHTTPServer.Serve(listener); err != nil && err != http.ErrServerClosed {
				rpcsLog.Errorf("Public RPC server: %v", err)
			}
		}(listener)
	}
}

func (s *rpcServer) stopPublicRPC() {
	if s.publicHTTPServer == nil {
		return
	}
	// Wait for actual handler completion before the chain/database shuts down.
	// A disconnected client does not cancel the accumulator's Prove operation.
	if err := s.publicHTTPServer.Shutdown(context.Background()); err != nil {
		rpcsLog.Errorf("Problem shutting down public RPC: %v", err)
	}
	// Also close listeners if Stop was called before Serve registered them.
	for _, listener := range s.cfg.PublicListeners {
		listener.Close()
	}
}

// allow applies one shared budget across listeners and batch items. Failed
// requests consume the budget too; it requires no per-client tracking map.
func (h *publicRPCHandler) allow(count int) bool {
	h.rateMu.Lock()
	defer h.rateMu.Unlock()
	now := time.Now()
	h.tokens += now.Sub(h.last).Seconds() * publicRPCRate
	if h.tokens > publicRPCRate {
		h.tokens = publicRPCRate
	}
	h.last = now
	if h.tokens < float64(count) {
		return false
	}
	h.tokens -= float64(count)
	return true
}

func (h *publicRPCHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// WriteTimeout alone does not cancel a handler. Stop admitting more batch
	// work after the deadline, while retaining slots for work already running.
	ctx, cancel := context.WithTimeout(r.Context(), publicRPCTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	r.Close = true
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Use POST", http.StatusMethodNotAllowed)
		return
	}
	select {
	case h.requests <- struct{}{}:
		defer func() { <-h.requests }()
	default:
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Public RPC busy", http.StatusServiceUnavailable)
		return
	}
	if !h.allow(1) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Public RPC rate limit", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, publicRPCMaxBody)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "Request too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "Cannot read request", http.StatusBadRequest)
		}
		return
	}
	body = bytes.TrimSpace(body)
	if !json.Valid(body) {
		w.Write(publicRPCReply(btcjson.RpcVersion1, nil, nil, btcjson.ErrRPCParse))
		return
	}
	if body[0] != '[' {
		reply := h.process(body, r.Context())
		if reply == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Write(append(reply, '\n'))
		return
	}

	var batch []json.RawMessage
	if err := json.Unmarshal(body, &batch); err != nil || len(batch) == 0 || len(batch) > publicRPCMaxBatch {
		w.Write(publicRPCReply(btcjson.RpcVersion2, nil, nil,
			btcjson.NewRPCError(btcjson.ErrRPCInvalidRequest.Code, "Batch must contain 1 to 8 calls")))
		return
	}
	if !h.allow(len(batch) - 1) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Public RPC rate limit", http.StatusTooManyRequests)
		return
	}
	replies := make([]json.RawMessage, 0, len(batch))
	for _, request := range batch {
		if r.Context().Err() != nil {
			return
		}
		if reply := h.process(request, r.Context()); reply != nil {
			replies = append(replies, reply)
		}
	}
	if len(replies) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// Each reply is bounded, and the batch contains at most eight replies.
	json.NewEncoder(w).Encode(replies)
}

func (h *publicRPCHandler) process(body []byte, ctx context.Context) json.RawMessage {
	// Use raw parameters and IDs to avoid float64 round trips. In particular,
	// a fractional vout must not get rounded to an integer before validation.
	var request struct {
		Version btcjson.RPCVersion `json:"jsonrpc"`
		Method  string             `json:"method"`
		Params  []json.RawMessage  `json:"params"`
		ID      json.RawMessage    `json:"id"`
	}
	request.Version = btcjson.RpcVersion1
	if len(body) == 0 || body[0] != '{' || json.Unmarshal(body, &request) != nil ||
		!request.Version.IsValid() || request.Method == "" {
		return publicRPCReply(btcjson.RpcVersion1, nil, nil, btcjson.ErrRPCInvalidRequest)
	}
	var id interface{}
	if len(request.ID) > 0 {
		if json.Unmarshal(request.ID, &id) != nil || !btcjson.IsValidIDType(id) {
			return publicRPCReply(request.Version, nil, nil, btcjson.ErrRPCInvalidRequest)
		}
	}
	// Match native read-only notification behavior: there is no work to do
	// when no response was requested. JSON-RPC 2.0 permits an explicit null ID.
	if len(request.ID) == 0 || (request.Version == btcjson.RpcVersion1 && id == nil) {
		return nil
	}
	reply := func(result interface{}, err error) json.RawMessage {
		return publicRPCReply(request.Version, request.ID, result, err)
	}
	if _, ok := rpcPublic[request.Method]; !ok {
		return reply(nil, btcjson.ErrRPCMethodNotFound)
	}
	parsed := parseCmd(&btcjson.Request{
		Jsonrpc: request.Version,
		Method:  request.Method,
		Params:  request.Params,
	})
	if parsed.err != nil {
		return reply(nil, parsed.err)
	}
	if err := validatePublicRPCCmd(parsed.cmd); err != nil {
		return reply(nil, err)
	}
	if ctx.Err() != nil {
		return nil
	}
	if request.Method == "proveutxochaintipinclusion" {
		select {
		case h.proofs <- struct{}{}:
			// Keep this slot until the native handler returns, even if the
			// client disconnects or the HTTP write deadline expires.
			defer func() { <-h.proofs }()
		default:
			return reply(nil, btcjson.NewRPCError(-32000, "Public proof service busy; retry later"))
		}
	}
	result, err := h.dispatch(parsed, ctx.Done())
	return reply(result, err)
}

func validatePublicRPCCmd(cmd interface{}) *btcjson.RPCError {
	invalid := func(message string) *btcjson.RPCError {
		return btcjson.NewRPCError(btcjson.ErrRPCInvalidParams.Code, message)
	}
	var hash string
	switch c := cmd.(type) {
	case *btcjson.GetBestBlockHashCmd:
		return nil
	case *btcjson.GetBlockHashCmd:
		if c.Index < 0 || c.Index > 0x7fffffff {
			return invalid("Height must be between 0 and 2147483647")
		}
		return nil
	case *btcjson.GetBlockHeaderCmd:
		hash = c.Hash
	case *btcjson.GetTxOutCmd:
		hash = c.Txid
	case *btcjson.GetUtreexoRootsCmd:
		hash = c.BlockHash
	case *btcjson.ProveUtxoChainTipInclusionCmd:
		if len(c.Txids) != 1 || len(c.Vouts) != 1 {
			return invalid("Public proofs require exactly one txid and one vout")
		}
		if c.Verbosity == nil || (*c.Verbosity != 0 && *c.Verbosity != 1) {
			return invalid("Verbosity must be 0 or 1")
		}
		hash = c.Txids[0]
	default:
		return btcjson.ErrRPCMethodNotFound
	}
	if len(hash) != 64 {
		return invalid("Hash must contain exactly 64 hexadecimal characters")
	}
	if _, err := hex.DecodeString(hash); err != nil {
		return invalid("Hash must contain exactly 64 hexadecimal characters")
	}
	return nil
}

func publicRPCReply(version btcjson.RPCVersion, id json.RawMessage, result interface{}, err error) json.RawMessage {
	var rpcErr *btcjson.RPCError
	if err != nil && !errors.As(err, &rpcErr) {
		rpcErr = btcjson.ErrRPCInternal
	}
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	response := struct {
		Version btcjson.RPCVersion `json:"jsonrpc"`
		Result  interface{}        `json:"result"`
		Error   *btcjson.RPCError  `json:"error,omitempty"`
		ID      json.RawMessage    `json:"id"`
	}{version, result, rpcErr, id}
	body, marshalErr := json.Marshal(response)
	if marshalErr != nil || len(body) > publicRPCMaxReply {
		response.Result = nil
		response.Error = btcjson.ErrRPCInternal
		body, _ = json.Marshal(response)
	}
	return body
}
