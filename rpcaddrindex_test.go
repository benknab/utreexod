package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/blockchain/indexers"
	"github.com/utreexo/utreexod/btcjson"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/mempool"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

// Serve real HTTP RPC against a validating regtest chain and proof index while
// the address worker has not processed a single block. No RPC or node fakes.
func TestRPCDuringAddressCatchUp(t *testing.T) {
	previousLog := logRotator
	initLogRotator(filepath.Join(t.TempDir(), "node.log"))
	t.Cleanup(func() {
		require.NoError(t, logRotator.Close())
		logRotator = previousLog
	})
	previous := cfg
	cfg = &config{RPCUser: "test", RPCPass: "test", RPCMaxClients: 10}
	t.Cleanup(func() { cfg = previous })
	params := chaincfg.RegressionNetParams
	db, err := database.Create("ffldb", filepath.Join(t.TempDir(), "blocks"), wire.MainNet)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	txIndex := indexers.NewTxIndex(db)
	addrIndex := indexers.NewAddrIndex(db, &params)
	worker := indexers.NewBackgroundAddrIndex(addrIndex)
	t.Cleanup(func() { require.NoError(t, worker.Stop()) })
	proofIndex, err := indexers.NewFlatUtreexoProofIndex(false, &params, 1<<20, t.TempDir(), db.Flush)
	require.NoError(t, err)
	chain, err := blockchain.New(&blockchain.Config{
		DB: db, ChainParams: &params, TimeSource: blockchain.NewMedianTime(),
		SigCache: txscript.NewSigCache(100), UtxoCacheMaxSize: 1 << 20,
		IndexManager: indexers.NewManager(db, []indexers.Indexer{txIndex, worker, proofIndex}),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, proofIndex.CloseUtreexoState()) })
	b1, _, err := blockchain.AddBlock(chain, btcutil.NewBlock(params.GenesisBlock), nil)
	require.NoError(t, err)
	b2, _, err := blockchain.AddBlock(chain, b1, nil)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rpc, err := newRPCServer(&rpcserverConfig{
		Listeners: []net.Listener{listener}, Chain: chain, DB: db,
		ChainParams: &params, TimeSource: blockchain.NewMedianTime(),
		TxIndex: txIndex, AddrIndex: worker,
		FlatUtreexoProofIndex: proofIndex, TxMemPool: mempool.New(&mempool.Config{ChainParams: &params}),
	})
	require.NoError(t, err)
	rpc.Start()
	t.Cleanup(func() { require.NoError(t, rpc.Stop()) })
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method string, args ...interface{}) btcjson.Response {
		t.Helper()
		body, err := json.Marshal(map[string]interface{}{"jsonrpc": "1.0", "id": 1, "method": method, "params": args})
		require.NoError(t, err)
		req, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String(), bytes.NewReader(body))
		require.NoError(t, err)
		req.SetBasicAuth("test", "test")
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var result btcjson.Response
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
		return result
	}
	success := func(method string, args ...interface{}) json.RawMessage {
		t.Helper()
		response := call(method, args...)
		require.Nil(t, response.Error, "%s: %v", method, response.Error)
		return response.Result
	}
	var status btcjson.AddrIndexInfoResult
	require.NoError(t, json.Unmarshal(success("getaddrindexinfo"), &status))
	require.Equal(t, int32(-1), status.Height)
	require.Equal(t, b2.Height(), status.ValidatedHeight)
	require.False(t, status.Synced)
	success("help", "getaddrindexinfo")

	_, addresses, _, err := txscript.ExtractPkScriptAddrs(params.GenesisBlock.Transactions[0].TxOut[0].PkScript, &params)
	require.NoError(t, err)
	address := addresses[0].EncodeAddress()
	// Fill the unconfirmed index so the reverse count=1 request could bypass
	// the database entirely if the early readiness check were removed.
	unconfirmed := btcutil.NewTx(wire.NewMsgTx(1))
	unconfirmed.MsgTx().AddTxOut(params.GenesisBlock.Transactions[0].TxOut[0])
	addrIndex.AddUnconfirmedTx(unconfirmed, blockchain.NewUtxoViewpoint())
	require.Len(t, addrIndex.UnconfirmedTxnsForAddress(addresses[0]), 1)
	for _, args := range [][]interface{}{
		{address}, {address, 0, 0, 0}, {address, 0, 0, 1, 0, true},
	} {
		result := call("searchrawtransactions", args...)
		require.NotNil(t, result.Error)
		require.Equal(t, btcjson.ErrRPCInWarmup, result.Error.Code)
		require.Contains(t, result.Error.Message, "getaddrindexinfo")
	}
	addrIndex.RemoveUnconfirmedTx(unconfirmed.Hash())

	// The calls needed for discovery alternatives and Greenhold proofs remain
	// available, and proof output verifies against the live accumulator.
	require.JSONEq(t, `"`+b2.Hash().String()+`"`, string(success("getbestblockhash")))
	success("getblock", b1.Hash().String())
	txid := b1.MsgBlock().Transactions[0].TxHash().String()
	success("getrawtransaction", txid)
	coin := success("gettxout", txid, 0, false)
	require.NotEqual(t, "null", string(coin))
	success("getutreexoroots", b2.Hash().String())
	var proof string
	require.NoError(t, json.Unmarshal(success("proveutxochaintipinclusion", []string{txid}, []uint32{0}, 0), &proof))
	require.Equal(t, "true", string(success("verifyutxochaintipinclusionproof", proof)))

	// Starting the worker changes only address-query availability.
	worker.Start()
	require.Eventually(t, func() bool {
		s, err := worker.Snapshot()
		return err == nil && s.Synced
	}, 5*time.Second, time.Millisecond)
	var history []string
	require.NoError(t, json.Unmarshal(success("searchrawtransactions", address, 0), &history))
	require.Len(t, history, 1)
}
