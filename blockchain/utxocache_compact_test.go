package blockchain

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

func TestUtxoCacheCompactionPersistence(t *testing.T) {
	db, err := database.Create("ffldb", filepath.Join(t.TempDir(), "db"), wire.MainNet)
	require.NoError(t, err)
	defer db.Close()
	ops := populateFetchEntries(t, db, 3)
	cache := newUtxoCache(db, 16<<20)
	_, err = cache.fetchEntries(ops)
	require.NoError(t, err)
	require.NoError(t, cache.addTxIn(&wire.TxIn{PreviousOutPoint: ops[0]}, nil))
	fresh := outpointFromInt(100)
	require.NoError(t, cache.addTxOut(fresh, &wire.TxOut{Value: 7, PkScript: []byte{txscript.OP_TRUE}}, false, 2))
	missing := outpointFromInt(101)
	_, err = cache.fetchEntry(missing)
	require.NoError(t, err)
	// Exercise actual fresh-spend churn, not just a fabricated byte counter.
	for i := 0; i < 1<<20; i++ {
		op := outpointFromInt(i + 1000)
		require.NoError(t, cache.addTxOut(op, &wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}}, false, 2))
		require.NoError(t, cache.addTxIn(&wire.TxIn{PreviousOutPoint: op}, nil))
	}
	before := cache.totalMemoryUsage()
	entries := make(map[wire.OutPoint]*UtxoEntry)
	for op, entry := range cache.cachedEntries.entries {
		entries[op] = entry
	}
	// The normal conditional flush must compact rather than empty the cache.
	require.NoError(t, cache.flushToDB(FlushIfNeeded, &BestState{}))
	require.Equal(t, entries, cache.cachedEntries.entries)
	for op, entry := range entries {
		require.True(t, cache.cachedEntries.entries[op] == entry)
	}
	require.Less(t, cache.totalMemoryUsage(), before/100)
	require.ElementsMatch(t, []wire.OutPoint{ops[0], fresh}, cache.dirtyOutpoints)
	require.False(t, cache.compactIfSparse(), "do not repeatedly rebuild a dense cache")

	// A failed commit still leaves dirty flags and entries available for retry.
	wantErr := errors.New("rollback")
	err = db.Update(func(tx database.Tx) error {
		_, err := cache.flush(tx, FlushSync, &BestState{})
		require.NoError(t, err)
		return wantErr
	})
	require.ErrorIs(t, err, wantErr)
	require.Len(t, cache.dirtyOutpoints, 2)
	require.NoError(t, cache.flushToDB(FlushRequired, &BestState{}))
	require.Zero(t, cache.cachedEntries.length())
	read := newUtxoCache(db, 16<<20)
	got, err := read.fetchEntries([]wire.OutPoint{ops[0], ops[1], fresh, missing})
	require.NoError(t, err)
	require.Nil(t, got[0])
	require.EqualValues(t, 2, got[1].Amount())
	require.EqualValues(t, 7, got[2].Amount())
	require.Nil(t, got[3])
}

func BenchmarkUtxoCacheCompaction(b *testing.B) {
	for range b.N {
		b.StopTimer()
		cache := newUtxoCache(nil, 256<<20)
		for i := 0; i < 1<<20; i++ {
			op := outpointFromInt(i)
			cache.cachedEntries.put(op, nil)
			cache.dirtyOutpoints = append(cache.dirtyOutpoints, op)
		}
		for i := 0; i < 3*(1<<18); i++ {
			cache.cachedEntries.delete(outpointFromInt(i))
		}
		before := cache.totalMemoryUsage()
		b.StartTimer()
		if !cache.compactIfSparse() {
			b.Fatal("expected compaction")
		}
		b.StopTimer()
		b.ReportMetric(float64(before-cache.totalMemoryUsage()), "charged-bytes-freed/op")
		b.StartTimer()
	}
}
