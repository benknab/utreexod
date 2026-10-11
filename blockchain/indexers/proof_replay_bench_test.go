package indexers

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

// benchmarkReplayChain creates a regtest backlog with thousands of external
// spends per block. Validation/setup is outside the proof replay timer.
func benchmarkReplayChain(t testing.TB) *proofWorkerFixture {
	f := proofWorkerFixtureAt(t, t.TempDir(), true)
	first, coins := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
	funding := wire.NewMsgTx(1)
	funding.AddTxIn(&wire.TxIn{PreviousOutPoint: coins[0].PrevOut, Sequence: wire.MaxTxInSequenceNum})
	const fanout = 2500
	for range fanout {
		funding.AddTxOut(&wire.TxOut{Value: int64(coins[0].Amount)/fanout - 1, PkScript: []byte{txscript.OP_TRUE}})
	}
	base, _ := blockchain.NewBlock(f.chain, first, nil)
	msg := base.MsgBlock()
	msg.Transactions = append(msg.Transactions, funding)
	txs := []*btcutil.Tx{btcutil.NewTx(msg.Transactions[0]), btcutil.NewTx(funding)}
	merkles := blockchain.BuildMerkleTreeStore(txs, false)
	msg.Header.MerkleRoot = *merkles[len(merkles)-1]
	require.True(t, blockchain.SolveBlock(&msg.Header))
	tip := btcutil.NewBlock(msg)
	tip.SetHeight(2)
	_, _, err := f.chain.ProcessBlock(tip, blockchain.BFNone)
	require.NoError(t, err)
	hash := funding.TxHash()
	spends := make([]*blockchain.SpendableOut, fanout)
	for i := range spends {
		spends[i] = &blockchain.SpendableOut{PrevOut: wire.OutPoint{Hash: hash, Index: uint32(i)}, Amount: btcutil.Amount(funding.TxOut[i].Value)}
	}
	for range 32 {
		tip, spends = f.add(tip, spends)
	}
	require.NoError(t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
	return f
}

func BenchmarkBackgroundProofReplay(b *testing.B) {
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		f := benchmarkReplayChain(b)
		b.StartTimer()
		f.waitSynced()
		b.StopTimer()
		f.closeWorker()
	}
}

func BenchmarkProofTTLRecovery(b *testing.B) {
	f := benchmarkReplayChain(b)
	f.waitSynced()
	f.closeWorker()
	f.openWorker()
	require.NoError(b, f.worker.recover())
	block, err := f.chain.BlockByHeight(3)
	require.NoError(b, err)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		require.NoError(b, f.worker.repairTTLs(block, true))
	}
}

func TestProofTTLRecoveryFanout(t *testing.T) {
	f := benchmarkReplayChain(t)
	f.waitSynced()
	f.closeWorker()
	f.openWorker()
	require.NoError(t, f.worker.recover())
	block, err := f.chain.BlockByHeight(3)
	require.NoError(t, err)
	before, err := f.worker.ttlState.FetchData(2)
	require.NoError(t, err)
	require.NoError(t, f.worker.repairTTLs(block, false))
	cleared, err := f.worker.ttlState.FetchData(2)
	require.NoError(t, err)
	require.Equal(t, make([]byte, len(before)), cleared)
	require.NoError(t, f.worker.repairTTLs(block, true))
	after, err := f.worker.ttlState.FetchData(2)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
