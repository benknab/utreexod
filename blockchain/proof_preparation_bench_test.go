package blockchain

import (
	"fmt"
	"testing"

	"github.com/utreexo/utreexo"
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

func BenchmarkProofAdditionHashes(b *testing.B) {
	tx := wire.NewMsgTx(1)
	for i := range 5000 {
		tx.AddTxOut(&wire.TxOut{Value: int64(i), PkScript: []byte{txscript.OP_TRUE}})
	}
	block := btcutil.NewBlock(&wire.MsgBlock{Transactions: []*wire.MsgTx{tx}})
	for _, direct := range []bool{false, true} {
		b.Run(fmt.Sprintf("direct=%t", direct), func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				var hashes []utreexo.Leaf
				if direct {
					hashes = BlockToAddHashes(block, nil, 5000)
				} else {
					leaves := BlockToAddLeaves(block, nil, 5000)
					hasher := wire.NewLeafHasher()
					hashes = make([]utreexo.Leaf, len(leaves))
					for i := range leaves {
						hashes[i].Hash = hasher.HashLeaf(&leaves[i])
					}
				}
				if len(hashes) != 5000 {
					b.Fatal("wrong add count")
				}
			}
		})
	}
}

func TestDirectAdditionHashes(t *testing.T) {
	tx := wire.NewMsgTx(1)
	for i := range 100 {
		script := []byte{txscript.OP_TRUE}
		if i%4 == 0 {
			script = []byte{txscript.OP_RETURN}
		}
		tx.AddTxOut(&wire.TxOut{Value: int64(i), PkScript: script})
	}
	block := btcutil.NewBlock(&wire.MsgBlock{Transactions: []*wire.MsgTx{tx}})
	block.SetHeight(10)
	skip := []uint32{3, 11, 19}
	leaves := BlockToAddLeaves(block, skip, 100)
	hashes := BlockToAddHashes(block, skip, 100)
	if len(hashes) != 72 || len(hashes) != len(leaves) {
		t.Fatal("incorrect filtered additions")
	}
	for i := range leaves {
		if hashes[i].Hash != leaves[i].LeafHash() || hashes[i].Remember {
			t.Fatalf("addition %d differs", i)
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
