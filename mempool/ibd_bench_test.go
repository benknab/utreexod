package mempool

import (
	"testing"

	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/wire"
)

// BenchmarkEmptyPoolBlockTransaction models the mempool calls made by the
// sync manager for each confirmed transaction during IBD.
func BenchmarkEmptyPoolBlockTransaction(b *testing.B) {
	mp := new(TxPool)
	msg := wire.NewMsgTx(2)
	for i := 0; i < 100; i++ {
		msg.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i)}})
		msg.AddTxOut(&wire.TxOut{Value: 1})
	}
	tx := btcutil.NewTx(msg)
	_ = tx.Hash()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mp.RemoveTransaction(tx, false)
		mp.RemoveDoubleSpends(tx)
		mp.RemoveOrphan(tx)
		mp.ProcessOrphans(tx)
	}
}
