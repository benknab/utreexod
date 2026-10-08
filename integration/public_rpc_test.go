package integration

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexo"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcjson"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/integration/rpctest"
	"github.com/utreexo/utreexod/wire"
)

// TestPublicRPC exercises the real TLS listener, all six native methods and
// both proof indexes. The proof is verified locally against freshly fetched
// roots, including after the node advances to another block.
func TestPublicRPC(t *testing.T) {
	blocks, _, err := loadTestBlocks("blocks.txt")
	require.NoError(t, err)
	for _, index := range []string{"--flatutreexoproofindex", "--utreexoproofindex"} {
		t.Run(index, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			address := listener.Addr().String()
			require.NoError(t, listener.Close())
			node, err := rpctest.New(&chaincfg.RegressionNetParams, nil, []string{
				index, "--noutreexo", "--nobdkwallet", "--prune=0",
				"--rpcpubliclisten=" + address,
			}, "")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, node.TearDown()) })
			require.NoError(t, node.SetUp(false, 0))
			for _, block := range blocks[1:11] {
				require.NoError(t, node.Client.SubmitBlock(block, nil))
			}

			rpcConfig := node.RPCConfig()
			certs := x509.NewCertPool()
			require.True(t, certs.AppendCertsFromPEM(rpcConfig.Certificates))
			transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: certs, MinVersion: tls.VersionTLS12}}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
			call := func(method string, params []interface{}, result interface{}) {
				t.Helper()
				request, err := btcjson.NewRequest(btcjson.RpcVersion1, 1, method, params)
				require.NoError(t, err)
				body, err := json.Marshal(request)
				require.NoError(t, err)
				response, err := client.Post("https://"+address+"/", "application/json", bytes.NewReader(body))
				require.NoError(t, err)
				defer response.Body.Close()
				require.Equal(t, http.StatusOK, response.StatusCode)
				var reply btcjson.Response
				require.NoError(t, json.NewDecoder(response.Body).Decode(&reply))
				require.Nil(t, reply.Error, "%s: %v", method, reply.Error)
				require.NoError(t, json.Unmarshal(reply.Result, result))
			}

			var tip, genesis string
			call("getbestblockhash", nil, &tip)
			require.Equal(t, blocks[10].Hash().String(), tip)
			call("getblockhash", []interface{}{0}, &genesis)
			require.Equal(t, chaincfg.RegressionNetParams.GenesisHash.String(), genesis)
			var header btcjson.GetBlockHeaderVerboseResult
			call("getblockheader", []interface{}{tip, true}, &header)
			require.Equal(t, int32(10), header.Height)
			var rawHeader string
			call("getblockheader", []interface{}{tip, false}, &rawHeader)
			require.Len(t, rawHeader, 160)

			tx := blocks[10].MsgBlock().Transactions[0]
			txid := tx.TxHash()
			var coin btcjson.GetTxOutResult
			call("gettxout", []interface{}{txid.String(), 0, false}, &coin)
			require.Equal(t, tip, coin.BestBlock)
			require.True(t, coin.Coinbase)
			var verbose btcjson.ProveUtxoChainTipInclusionVerboseResult
			call("proveutxochaintipinclusion", []interface{}{[]string{txid.String()}, []uint32{0}, 1}, &verbose)
			require.Equal(t, tip, verbose.ProvedAtHash)
			var proof blockchain.ChainTipProof
			require.NoError(t, proof.DecodeString(verbose.Hex))
			require.Equal(t, tip, proof.ProvedAtHash.String())
			require.Len(t, proof.HashesProven, 1)
			amount, err := btcutil.NewAmount(coin.Value)
			require.NoError(t, err)
			script, err := hex.DecodeString(coin.ScriptPubKey.Hex)
			require.NoError(t, err)
			leaf := wire.LeafData{
				BlockHash: *blocks[10].Hash(), OutPoint: wire.OutPoint{Hash: txid, Index: 0},
				Height: 10, IsCoinBase: coin.Coinbase, Amount: int64(amount), PkScript: script,
			}
			require.Equal(t, utreexo.Hash(leaf.LeafHash()), proof.HashesProven[0])
			require.Equal(t, chainhash.Hash(proof.HashesProven[0]).String(), verbose.HashesProven[0])
			require.Equal(t, proof.AccProof.Targets, verbose.ProofTargets)

			verify := func() {
				t.Helper()
				var state btcjson.GetUtreexoRootsResult
				call("getutreexoroots", []interface{}{tip}, &state)
				stump := utreexo.Stump{NumLeaves: state.NumLeaves}
				for _, root := range state.Roots {
					raw, err := hex.DecodeString(root)
					require.NoError(t, err)
					require.Len(t, raw, 32)
					var hash utreexo.Hash
					copy(hash[:], raw)
					stump.Roots = append(stump.Roots, hash)
				}
				_, err := utreexo.Verify(stump, proof.HashesProven, *proof.AccProof)
				require.NoError(t, err)
			}
			verify()
			require.NoError(t, node.Client.SubmitBlock(blocks[11], nil))
			verify()

			// The public listener cannot be elevated with valid admin credentials.
			body := []byte(`{"jsonrpc":"1.0","id":1,"method":"stop","params":[]}`)
			request, err := http.NewRequest(http.MethodPost, "https://"+address+"/", bytes.NewReader(body))
			require.NoError(t, err)
			request.SetBasicAuth(rpcConfig.User, rpcConfig.Pass)
			response, err := client.Do(request)
			require.NoError(t, err)
			var denied btcjson.Response
			require.NoError(t, json.NewDecoder(response.Body).Decode(&denied))
			response.Body.Close()
			require.NotNil(t, denied.Error)
			require.Equal(t, btcjson.ErrRPCMethodNotFound.Code, denied.Error.Code)

			// The original listener still requires authentication.
			response, err = client.Post("https://"+rpcConfig.Host+"/", "application/json", bytes.NewReader(body))
			require.NoError(t, err)
			response.Body.Close()
			require.Equal(t, http.StatusUnauthorized, response.StatusCode)
			height, err := node.Client.GetBlockCount()
			require.NoError(t, err)
			require.Equal(t, int64(11), height)
		})
	}
}
