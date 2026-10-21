package blockchain

import (
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/txscript"
)

type countingPrefetchDB struct {
	database.DB
	views int
}

func (db *countingPrefetchDB) View(fn func(database.Tx) error) error {
	db.views++
	return db.DB.View(fn)
}

func prefetchBlockFixture(t testing.TB) (*countingPrefetchDB, *btcutil.Block) {
	t.Helper()
	db, err := database.Create("ffldb", filepath.Join(t.TempDir(), "db"), wire.MainNet)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	inputs := populateFetchEntries(t, db, 4096)
	require.NoError(t, db.Flush())
	coinbase := wire.NewMsgTx(2)
	coinbase.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: ^uint32(0)}, []byte{1, 2}, nil))
	coinbase.AddTxOut(&wire.TxOut{Value: 50, PkScript: []byte{txscript.OP_TRUE}})
	tx := wire.NewMsgTx(2)
	for i := range inputs {
		tx.AddTxIn(wire.NewTxIn(&inputs[i], nil, nil))
	}
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	child := wire.NewMsgTx(2)
	child.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: tx.TxHash()}, nil, nil))
	child.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	block := btcutil.NewBlock(&wire.MsgBlock{Transactions: []*wire.MsgTx{coinbase, tx, child}})
	block.SetHeight(2)
	return &countingPrefetchDB{DB: db}, block
}

func TestPrefetchBlockInputs(t *testing.T) {
	db, block := prefetchBlockFixture(t)
	serial := newUtxoCache(db, 16<<20)
	var expected []SpentTxOut
	require.NoError(t, serial.connectTransactions(block, &expected))
	require.Equal(t, 4096, db.views)

	cache := newUtxoCache(db, 16<<20)
	db.views = 0
	require.NoError(t, cache.prefetchBlockInputs(block))
	require.Equal(t, 1, db.views)
	require.Empty(t, cache.dirtyOutpoints)
	for _, tx := range block.Transactions() {
		_, found := cache.cachedEntries.get(wire.OutPoint{Hash: *tx.Hash()})
		require.False(t, found, "prefetch must not publish same-block outputs or misses")
	}
	var actual []SpentTxOut
	require.NoError(t, cache.connectTransactions(block, &actual))
	require.Equal(t, 1, db.views)
	require.Equal(t, expected, actual)
	require.Equal(t, serial.cachedEntries.entries, cache.cachedEntries.entries)
	require.Equal(t, serial.totalEntryMemory, cache.totalEntryMemory)
	require.Equal(t, serial.dirtyOutpoints, cache.dirtyOutpoints)
}

func BenchmarkPrefetchBlockInputs(b *testing.B) {
	db, block := prefetchBlockFixture(b)
	for _, name := range []string{"single", "batch"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				cache := newUtxoCache(db, 16<<20)
				if name == "batch" {
					require.NoError(b, cache.prefetchBlockInputs(block))
				}
				var stxos []SpentTxOut
				require.NoError(b, cache.connectTransactions(block, &stxos))
			}
		})
	}
}
