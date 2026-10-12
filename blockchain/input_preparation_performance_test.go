package blockchain

import (
	"fmt"
	"math"
	"reflect"
	"testing"

	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

func inputFetchBenchmarkTx(inputs int) *btcutil.Tx {
	tx := wire.NewMsgTx(2)
	for i := range inputs {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i)}})
	}
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	return btcutil.NewTx(tx)
}

func inputFetchBenchmarkBlock(outputs int, sameBlock bool) *btcutil.Block {
	funding := inputFetchBenchmarkTx(1).MsgTx()
	funding.TxOut = nil
	for range outputs {
		funding.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	}
	spend := inputFetchBenchmarkTx(outputs).MsgTx()
	hash := funding.TxHash()
	for _, in := range spend.TxIn {
		in.PreviousOutPoint.Hash = hash
	}
	txs := []*wire.MsgTx{inputFetchBenchmarkTx(1).MsgTx()}
	if sameBlock {
		txs = append(txs, funding)
	}
	txs = append(txs, spend)
	block := btcutil.NewBlock(&wire.MsgBlock{Transactions: txs})
	block.SetHeight(100)
	for _, tx := range block.Transactions() {
		tx.Hash()
	}
	return block
}

func BenchmarkFindInputsToFetch(b *testing.B) {
	for _, sameBlock := range []bool{false, true} {
		for _, outputs := range []int{100, 2500} {
			b.Run(fmt.Sprintf("sameblock=%t/%d", sameBlock, outputs), func(b *testing.B) {
				block := inputFetchBenchmarkBlock(outputs, sameBlock)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					view := NewUtxoViewpoint()
					view.findInputsToFetch(block)
				}
			})
		}
	}
}

// This intentionally retains the original repeated-output-loading algorithm
// as an oracle for view contents and fetch order, including invalid inputs.
func referenceFindInputs(view *UtxoViewpoint, block *btcutil.Block) []wire.OutPoint {
	txs := block.Transactions()
	inFlight := make(map[chainhash.Hash]int)
	for i, tx := range txs {
		inFlight[*tx.Hash()] = i
	}
	needed := make([]wire.OutPoint, 0, len(txs))
	for i, tx := range txs[1:] {
		for _, in := range tx.MsgTx().TxIn {
			if index, ok := inFlight[in.PreviousOutPoint.Hash]; ok && i >= index {
				view.AddTxOuts(txs[index], block.Height())
				continue
			}
			if _, ok := view.entries[in.PreviousOutPoint]; !ok {
				needed = append(needed, in.PreviousOutPoint)
			}
		}
	}
	return needed
}

func TestFindInputsToFetchMatchesReference(t *testing.T) {
	for _, sameBlock := range []bool{false, true} {
		for _, outputs := range []int{1, 2, 100} {
			block := inputFetchBenchmarkBlock(outputs, sameBlock)
			// Include an unspendable source output, an out-of-range spend,
			// a repeated spend, and references to a later transaction.
			msg := block.MsgBlock()
			if sameBlock {
				msg.Transactions[1].TxOut[0].PkScript = []byte{txscript.OP_RETURN}
				hash := msg.Transactions[1].TxHash()
				for _, in := range msg.Transactions[2].TxIn {
					in.PreviousOutPoint.Hash = hash
				}
			}
			last := msg.Transactions[len(msg.Transactions)-1]
			last.AddTxIn(&wire.TxIn{PreviousOutPoint: last.TxIn[0].PreviousOutPoint})
			last.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{
				Hash: last.TxIn[0].PreviousOutPoint.Hash, Index: math.MaxUint32,
			}})
			msg.Transactions[0].AddTxOut(&wire.TxOut{Value: 2, PkScript: []byte{txscript.OP_TRUE}})
			last.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: msg.Transactions[0].TxHash(), Index: 1}})
			if sameBlock {
				future := inputFetchBenchmarkTx(1).MsgTx()
				future.LockTime = 123
				msg.Transactions[1].AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: future.TxHash()}})
				hash := msg.Transactions[1].TxHash()
				for _, in := range last.TxIn[:len(last.TxIn)-1] {
					in.PreviousOutPoint.Hash = hash
				}
				msg.Transactions = append(msg.Transactions, future)
			}
			// Re-wrap after mutations so transaction IDs are not stale.
			block = btcutil.NewBlock(msg)
			block.SetHeight(100)
			got, want := NewUtxoViewpoint(), NewUtxoViewpoint()
			for _, view := range []*UtxoViewpoint{got, want} {
				view.entries[wire.OutPoint{Index: 77}] = nil
				view.entries[last.TxIn[0].PreviousOutPoint] = &UtxoEntry{amount: 999, packedFlags: tfSpent}
			}
			gotNeeded := got.findInputsToFetch(block)
			wantNeeded := referenceFindInputs(want, block)
			if !reflect.DeepEqual(gotNeeded, wantNeeded) || !reflect.DeepEqual(got.entries, want.entries) {
				t.Fatalf("sameBlock=%t outputs=%d: input preparation differs", sameBlock, outputs)
			}
		}
	}
}
