package blockchain

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/wire"
)

func TestUtxoCacheReplacementMemory(t *testing.T) {
	s := newUtxoCache(nil, 1<<20)
	op := outpointFromInt(1)
	out := &wire.TxOut{Value: 1, PkScript: getValidP2PKHScript()}
	require.NoError(t, s.addTxOut(op, out, false, 1))
	first := s.totalEntryMemory
	for i := 0; i < 100; i++ {
		require.NoError(t, s.addTxOut(op, out, false, 1))
	}
	require.Equal(t, first, s.totalEntryMemory, "replacing an outpoint must not charge the old entry again")
}

func TestUtxoCacheChurnBudget(t *testing.T) {
	s := newUtxoCache(nil, 1<<20)
	before := s.totalMemoryUsage()
	out := &wire.TxOut{Value: 1, PkScript: getValidP2PKHScript()}
	for i := 0; i < 20000; i++ {
		op := outpointFromInt(i)
		require.NoError(t, s.addTxOut(op, out, false, 1))
		require.NoError(t, s.addTxIn(&wire.TxIn{PreviousOutPoint: op}, nil))
	}
	require.Zero(t, s.cachedEntries.length())
	require.Zero(t, s.totalEntryMemory)
	require.Greater(t, s.totalMemoryUsage(), before, "deletions must not hide map growth and tombstones")
	s.cachedEntries.clear()
	require.Zero(t, s.totalMemoryUsage())
	require.Nil(t, s.cachedEntries.entries, "full flush must release backing tables")
}

func TestUtxoCacheRepeatedFetchMemory(t *testing.T) {
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	s := chain.utxoCache
	op := outpointFromInt(1)
	out := &wire.TxOut{Value: 1, PkScript: getValidP2PKHScript()}
	require.NoError(t, s.addTxOut(op, out, false, 1))
	require.NoError(t, s.flushToDB(FlushRequired, chain.stateSnapshot))
	entries, err := s.fetchEntries([]wire.OutPoint{op, op, op})
	require.NoError(t, err)
	require.Same(t, entries[0], entries[1])
	require.Same(t, entries[0], entries[2])
	require.Equal(t, entries[0].memoryUsage(), s.totalEntryMemory)
	entries[0].Spend()
	require.True(t, entries[2].IsSpent())
}

var benchmarkUtxoCache *utxoCache

func BenchmarkUtxoCacheStartup(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchmarkUtxoCache = newUtxoCache(nil, 512<<20)
	}
	b.Cleanup(func() { benchmarkUtxoCache = nil })
}

func BenchmarkUtxoCacheResidentMemory(b *testing.B) {
	for i := 0; i < b.N; i++ {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		s := newUtxoCache(nil, 512<<20)
		out := &wire.TxOut{Value: 1, PkScript: getValidP2PKHScript()}
		for j := 0; j < 100000; j++ {
			if err := s.addTxOut(outpointFromInt(j), out, false, 1); err != nil {
				b.Fatal(err)
			}
		}
		// Replace half the live set to exercise retained tables after deletes.
		for j := 0; j < 50000; j++ {
			if err := s.addTxIn(&wire.TxIn{PreviousOutPoint: outpointFromInt(j)}, nil); err != nil {
				b.Fatal(err)
			}
			if err := s.addTxOut(outpointFromInt(j+100000), out, false, 1); err != nil {
				b.Fatal(err)
			}
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(s.totalMemoryUsage()), "budget-B")
		b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc)), "heap-B")
		runtime.KeepAlive(s)
	}
}
