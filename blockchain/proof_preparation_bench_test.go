package blockchain

import (
	"testing"

	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

func BenchmarkBlockToDelLeaves(b *testing.B) {
	parent := newBlockNode(&chaincfg.MainNetParams.GenesisBlock.Header, nil)
	chain := &BlockChain{
		index:     newBlockIndex(nil, &chaincfg.MainNetParams),
		bestChain: newChainView(parent),
	}
	chain.index.AddNode(parent)
	const count = 5000
	tx := wire.NewMsgTx(1)
	stxos := make([]SpentTxOut, count)
	for i := range stxos {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i)}})
		stxos[i] = SpentTxOut{Amount: 1, PkScript: []byte{txscript.OP_TRUE}, Height: 0}
	}
	block := btcutil.NewBlock(&wire.MsgBlock{
		Header: wire.BlockHeader{PrevBlock: parent.hash},
		Transactions: []*wire.MsgTx{
			{TxIn: []*wire.TxIn{{}}}, tx,
		},
	})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		leaves, err := BlockToDelLeaves(stxos, chain, block, nil)
		if err != nil || len(leaves) != count {
			b.Fatalf("got %d leaves: %v", len(leaves), err)
		}
	}
}

func BenchmarkProofDedupe(b *testing.B) {
	msg := &wire.MsgBlock{Transactions: []*wire.MsgTx{{TxIn: []*wire.TxIn{{}}, TxOut: []*wire.TxOut{{Value: 1}}}}}
	for i := range 2500 {
		tx := wire.NewMsgTx(2)
		for j := range 2 {
			tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i*2 + j)}})
			tx.AddTxOut(&wire.TxOut{Value: int64(i + 1), PkScript: []byte{txscript.OP_TRUE}})
		}
		if i > 0 && i%2 == 0 {
			tx.TxIn[0].PreviousOutPoint = wire.OutPoint{Hash: msg.Transactions[i].TxHash()}
		}
		msg.Transactions = append(msg.Transactions, tx)
	}
	block := btcutil.NewBlock(msg)
	// Transaction IDs are cached in both measured versions.
	DedupeBlock(block)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ins, outs, skip, _ := DedupeBlock(block)
		if ins != 5001 || outs != 5001 || len(skip) != 1249 {
			b.Fatalf("unexpected deduplication: %d %d %d", ins, outs, len(skip))
		}
	}
}
