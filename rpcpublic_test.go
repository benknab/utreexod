package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/btcjson"
)

const publicTestHash = "0000000000000000000000000000000000000000000000000000000000000001"

func publicTestHandler(dispatch func(*parsedRPCCmd, <-chan struct{}) (interface{}, error)) *publicRPCHandler {
	return newPublicRPCServer(dispatch).Handler.(*publicRPCHandler)
}

func publicTestRequest(handler http.Handler, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)))
	return w
}

func publicTestError(t *testing.T, w *httptest.ResponseRecorder, code btcjson.RPCErrorCode) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var response btcjson.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.NotNil(t, response.Error, w.Body.String())
	require.Equal(t, code, response.Error.Code)
}

// Every public method reaches the native typed command dispatcher without
// authentication. Numeric IDs and native uint64 results survive unchanged.
func TestPublicRPCNativeMethods(t *testing.T) {
	tests := []struct {
		method string
		params string
	}{
		{"getbestblockhash", "[]"},
		{"getblockhash", "[0]"},
		{"getblockheader", fmt.Sprintf(`[%q,false]`, publicTestHash)},
		{"gettxout", fmt.Sprintf(`[%q,4294967295,false]`, publicTestHash)},
		{"getutreexoroots", fmt.Sprintf(`[%q]`, publicTestHash)},
		{"proveutxochaintipinclusion", fmt.Sprintf(`[[%q],[0],1]`, publicTestHash)},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			calls := 0
			h := publicTestHandler(func(cmd *parsedRPCCmd, _ <-chan struct{}) (interface{}, error) {
				calls++
				require.Equal(t, test.method, cmd.method)
				require.NotNil(t, cmd.cmd)
				if proof, ok := cmd.cmd.(*btcjson.ProveUtxoChainTipInclusionCmd); ok {
					require.Equal(t, []string{publicTestHash}, proof.Txids)
					require.Equal(t, []uint32{0}, proof.Vouts)
				}
				return map[string]uint64{"target": 9007199254740993}, nil
			})
			w := publicTestRequest(h, fmt.Sprintf(`{"jsonrpc":"1.0","id":9007199254740993,"method":%q,"params":%s}`, test.method, test.params))
			require.Equal(t, http.StatusOK, w.Code)
			require.JSONEq(t, `{"jsonrpc":"1.0","id":9007199254740993,"result":{"target":9007199254740993}}`, w.Body.String())
			require.Contains(t, w.Body.String(), `"id":9007199254740993`)
			require.Contains(t, w.Body.String(), `"target":9007199254740993`)
			require.Equal(t, 1, calls)
		})
	}
}

func TestPublicRPCForbiddenMethods(t *testing.T) {
	for _, method := range []string{
		"stop", "invalidateblock", "reconsiderblock", "sendrawtransaction",
		"getmnemonicwords", "getpeerinfo", "getutreexoproof", "help", "unknown",
	} {
		t.Run(method, func(t *testing.T) {
			h := publicTestHandler(func(*parsedRPCCmd, <-chan struct{}) (interface{}, error) {
				t.Fatal("forbidden method reached dispatcher")
				return nil, nil
			})
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
				fmt.Sprintf(`{"id":1,"method":%q,"params":[]}`, method)))
			// Even credentials do not elevate a public request.
			r.SetBasicAuth("admin", "password")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			publicTestError(t, w, btcjson.ErrRPCMethodNotFound.Code)
		})
	}
}

func TestPublicRPCInvalidRequests(t *testing.T) {
	tests := []struct {
		body string
		code btcjson.RPCErrorCode
	}{
		{`{`, btcjson.ErrRPCParse.Code},
		{`null`, btcjson.ErrRPCInvalidRequest.Code},
		{`42`, btcjson.ErrRPCInvalidRequest.Code},
		{`{}`, btcjson.ErrRPCInvalidRequest.Code},
		{`{"id":{},"method":"getbestblockhash"}`, btcjson.ErrRPCInvalidRequest.Code},
		{`{"jsonrpc":"3.0","id":1,"method":"getbestblockhash"}`, btcjson.ErrRPCInvalidRequest.Code},
		{`{"id":1,"method":"getblockhash","params":{}}`, btcjson.ErrRPCInvalidRequest.Code},
		{`{"id":1,"method":"getblockhash","params":[-1]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"getblockhash","params":[4294967296]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"getblockheader","params":["01"]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"getutreexoroots","params":["` + strings.Repeat("z", 64) + `"]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"gettxout","params":["` + publicTestHash + `",-1]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"gettxout","params":["` + publicTestHash + `",4294967296]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"gettxout","params":["` + publicTestHash + `",0.99999999999999999]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"proveutxochaintipinclusion","params":[[],[]]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"proveutxochaintipinclusion","params":[["` + publicTestHash + `"],[]]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"proveutxochaintipinclusion","params":[["` + publicTestHash + `","` + publicTestHash + `"],[0,0]]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"proveutxochaintipinclusion","params":[["` + publicTestHash + `"],[0],2]}`, btcjson.ErrRPCInvalidParams.Code},
		{`{"id":1,"method":"proveutxochaintipinclusion","params":[["` + publicTestHash + `"],[0],null]}`, btcjson.ErrRPCInvalidParams.Code},
		{`[]`, btcjson.ErrRPCInvalidRequest.Code},
		{`[` + strings.Repeat(`{"id":1,"method":"getbestblockhash"},`, publicRPCMaxBatch) + `{ "id":1,"method":"getbestblockhash"}]`, btcjson.ErrRPCInvalidRequest.Code},
	}
	for _, test := range tests {
		t.Run(test.body, func(t *testing.T) {
			h := publicTestHandler(func(*parsedRPCCmd, <-chan struct{}) (interface{}, error) {
				t.Fatal("invalid request reached dispatcher")
				return nil, nil
			})
			publicTestError(t, publicTestRequest(h, test.body), test.code)
		})
	}
}

func TestPublicRPCBatchAuthorization(t *testing.T) {
	calls := 0
	h := publicTestHandler(func(cmd *parsedRPCCmd, _ <-chan struct{}) (interface{}, error) {
		calls++
		require.Equal(t, "getbestblockhash", cmd.method)
		return publicTestHash, nil
	})
	w := publicTestRequest(h, ` [
		{"jsonrpc":"2.0","id":1,"method":"getbestblockhash"},
		{"jsonrpc":"2.0","id":2,"method":"invalidateblock","params":["`+publicTestHash+`"]},
		{"jsonrpc":"2.0","method":"stop"},
		null,
		{"jsonrpc":"2.0","id":null,"method":"getbestblockhash"}
	]`)
	var replies []btcjson.Response
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &replies))
	require.Len(t, replies, 4)
	require.Nil(t, replies[0].Error)
	require.Equal(t, btcjson.ErrRPCMethodNotFound.Code, replies[1].Error.Code)
	require.Equal(t, btcjson.ErrRPCInvalidRequest.Code, replies[2].Error.Code)
	require.Nil(t, replies[3].Error)
	require.Equal(t, 2, calls)
}

func TestPublicRPCHTTPLimits(t *testing.T) {
	h := publicTestHandler(func(*parsedRPCCmd, <-chan struct{}) (interface{}, error) {
		t.Fatal("rejected HTTP request reached dispatcher")
		return nil, nil
	})
	for _, test := range []struct {
		method, path, body string
		status             int
	}{
		{http.MethodGet, "/", "", http.StatusMethodNotAllowed},
		{http.MethodGet, "/ws", "", http.StatusNotFound},
		{http.MethodPost, "/ws", "", http.StatusNotFound},
		{http.MethodPost, "/", strings.Repeat(" ", publicRPCMaxBody+1), http.StatusRequestEntityTooLarge},
		{http.MethodPost, "/", `{"jsonrpc":"2.0","method":"getbestblockhash"}`, http.StatusNoContent},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		// Exercise the streaming limit rather than relying on Content-Length.
		r.ContentLength = -1
		h.ServeHTTP(w, r)
		require.Equal(t, test.status, w.Code)
	}
	for i := 0; i < publicRPCMaxRequests; i++ {
		h.requests <- struct{}{}
	}
	w := publicTestRequest(h, `{"id":1,"method":"getbestblockhash"}`)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	for i := 0; i < publicRPCMaxRequests; i++ {
		<-h.requests
	}
	h.rateMu.Lock()
	h.tokens = 0
	h.last = time.Now().Add(time.Hour)
	h.rateMu.Unlock()
	w = publicTestRequest(h, `{"id":1,"method":"getbestblockhash"}`)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
}

// Cancellation cannot free a proof slot while the accumulator is still busy.
// Cheap public reads remain available while a proof is running.
func TestPublicRPCDisconnectedProof(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var proofCalls int32
	h := publicTestHandler(func(cmd *parsedRPCCmd, _ <-chan struct{}) (interface{}, error) {
		if cmd.method == "proveutxochaintipinclusion" {
			if atomic.AddInt32(&proofCalls, 1) == 1 {
				close(started)
			}
			<-release
		}
		return publicTestHash, nil
	})
	body := `{"id":1,"method":"proveutxochaintipinclusion","params":[["` + publicTestHash + `"],[0]]}`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)).WithContext(ctx)
	go func() {
		h.ServeHTTP(httptest.NewRecorder(), r)
		close(finished)
	}()
	defer func() { close(release); <-finished }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("proof never started")
	}
	cancel()
	publicTestError(t, publicTestRequest(h, body), -32000)
	require.Equal(t, int32(1), atomic.LoadInt32(&proofCalls))
	w := publicTestRequest(h, `{"id":2,"method":"getbestblockhash"}`)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), publicTestHash)
}

func TestPublicRPCResponseLimit(t *testing.T) {
	h := publicTestHandler(func(*parsedRPCCmd, <-chan struct{}) (interface{}, error) {
		return strings.Repeat("x", publicRPCMaxReply), nil
	})
	w := publicTestRequest(h, `{"id":1,"method":"getbestblockhash"}`)
	publicTestError(t, w, btcjson.ErrRPCInternal.Code)
	require.Less(t, w.Body.Len(), publicRPCMaxReply)
}

func TestPublicRPCBatchRateLimit(t *testing.T) {
	h := publicTestHandler(func(*parsedRPCCmd, <-chan struct{}) (interface{}, error) {
		t.Fatal("batch exceeding the budget reached dispatcher")
		return nil, nil
	})
	h.tokens = 1
	w := publicTestRequest(h, `[
		{"id":1,"method":"getbestblockhash"},
		{"id":2,"method":"getbestblockhash"}
	]`)
	require.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestPublicRPCCanceledBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	h := publicTestHandler(func(*parsedRPCCmd, <-chan struct{}) (interface{}, error) {
		calls++
		cancel()
		return publicTestHash, nil
	})
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`[
		{"id":1,"method":"getbestblockhash"},
		{"id":2,"method":"getbestblockhash"}
	]`)).WithContext(ctx)
	h.ServeHTTP(httptest.NewRecorder(), r)
	require.Equal(t, 1, calls)
}
