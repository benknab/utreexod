package blockchain

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/wire"
)

type failedUpdateDB struct {
	database.DB
	err error
}

func (db failedUpdateDB) Update(fn func(database.Tx) error) error {
	return db.DB.Update(func(tx database.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return db.err
	})
}

func TestUtxoCacheFailedFlushRetainsState(t *testing.T) {
	for _, mode := range []FlushMode{FlushRequired, FlushSync} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			chain, _, cleanup := utxoCacheTestChain(t.Name())
			t.Cleanup(cleanup)
			cache := chain.utxoCache
			op := outpointFromInt(1)
			require.NoError(t, cache.addTxOut(op, &wire.TxOut{Value: 42, PkScript: getValidP2PKHScript()}, false, 1))
			beforeMemory := cache.totalMemoryUsage()
			beforeHash, beforeTime := cache.lastFlushHash, cache.lastFlushTime
			db := chain.db
			failure := errors.New("transaction failed after staging UTXOs")
			chain.db = failedUpdateDB{DB: db, err: failure}
			cache.db = chain.db
			t.Cleanup(func() { chain.db, cache.db = db, db })
			require.ErrorIs(t, chain.FlushUtxoCache(mode), failure)
			require.Equal(t, beforeMemory, cache.totalMemoryUsage())
			require.Equal(t, beforeHash, cache.lastFlushHash)
			require.Equal(t, beforeTime, cache.lastFlushTime)
			entry, found := cache.cachedEntries.get(op)
			require.True(t, found)
			require.True(t, entry.isModified())
			require.NoError(t, db.View(func(tx database.Tx) error {
				entry, err := dbFetchUtxoEntry(tx, tx.Metadata().Bucket(utxoSetBucketName), op)
				require.Nil(t, entry, "aborted writes must not become visible")
				return err
			}))
			chain.db, cache.db = db, db
			require.NoError(t, chain.FlushUtxoCache(mode))
			if mode == FlushRequired {
				require.Zero(t, cache.totalMemoryUsage())
			} else {
				entry, found := cache.cachedEntries.get(op)
				require.True(t, found)
				require.False(t, entry.isModified())
				require.False(t, entry.isFresh())
			}
			require.NoError(t, db.View(func(tx database.Tx) error {
				entry, err := dbFetchUtxoEntry(tx, tx.Metadata().Bucket(utxoSetBucketName), op)
				require.NotNil(t, entry)
				require.Equal(t, int64(42), entry.Amount())
				return err
			}))
		})
	}
}

func TestUtxoCacheWarmSync(t *testing.T) {
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	cache := chain.utxoCache
	outpoints := populateFetchEntries(t, chain.db, 400)
	clean, err := cache.fetchEntries(outpoints)
	require.NoError(t, err)
	op := outpointFromInt(500)
	require.NoError(t, cache.addTxOut(op, &wire.TxOut{Value: 42, PkScript: getValidP2PKHScript()}, false, 1))
	require.NoError(t, cache.addTxIn(&wire.TxIn{PreviousOutPoint: outpoints[0]}, nil))
	require.Len(t, cache.dirtyOutpoints, 2)
	// Also cache a nil miss: syncing must not need a database delete for it.
	missing := outpointFromInt(600)
	_, err = cache.fetchEntries([]wire.OutPoint{missing})
	require.NoError(t, err)
	require.NoError(t, chain.FlushUtxoCache(FlushSync))
	require.Empty(t, cache.dirtyOutpoints)
	entry, found := cache.cachedEntries.get(outpoints[1])
	require.True(t, found)
	require.Same(t, clean[1], entry, "clean entries must stay warm")
	_, found = cache.cachedEntries.get(outpoints[0])
	require.False(t, found)
	entry, found = cache.cachedEntries.get(missing)
	require.True(t, found)
	require.Nil(t, entry)
	entry, _ = cache.cachedEntries.get(op)
	require.False(t, entry.isModified())
	require.False(t, entry.isFresh(), "synced outputs must not be treated as fresh")

	// Spending a previously synchronized output must delete it on disk.
	require.NoError(t, cache.addTxIn(&wire.TxIn{PreviousOutPoint: op}, nil))
	require.NoError(t, chain.FlushUtxoCache(FlushSync))
	require.NoError(t, chain.db.View(func(tx database.Tx) error {
		bucket := tx.Metadata().Bucket(utxoSetBucketName)
		for _, spent := range []wire.OutPoint{op, outpoints[0]} {
			entry, err := dbFetchUtxoEntry(tx, bucket, spent)
			require.NoError(t, err)
			require.Nil(t, entry)
		}
		return nil
	}))
	// Even a requested warm sync releases the whole map at its memory limit.
	cache.maxTotalMemoryUsage = cache.totalMemoryUsage() - 1
	require.NoError(t, chain.FlushUtxoCache(FlushSync))
	require.Zero(t, cache.totalMemoryUsage())
	require.Nil(t, cache.cachedEntries.entries)
}

func TestUtxoCacheWarmReplacement(t *testing.T) {
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	cache := chain.utxoCache
	op := outpointFromInt(1)
	out := &wire.TxOut{Value: 42, PkScript: getValidP2PKHScript()}
	require.NoError(t, cache.addTxOut(op, out, false, 1))
	require.NoError(t, chain.FlushUtxoCache(FlushSync))
	require.NoError(t, cache.addTxOut(op, out, false, 2))
	require.NoError(t, cache.addTxIn(&wire.TxIn{PreviousOutPoint: op}, nil))
	require.NoError(t, chain.FlushUtxoCache(FlushSync))
	require.NoError(t, chain.db.View(func(tx database.Tx) error {
		entry, err := dbFetchUtxoEntry(tx, tx.Metadata().Bucket(utxoSetBucketName), op)
		require.Nil(t, entry, "spending a replacement must remove the persisted predecessor")
		return err
	}))
}

func TestUtxoCacheHourlySync(t *testing.T) {
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	cache := chain.utxoCache
	op := outpointFromInt(1)
	require.NoError(t, cache.addTxOut(op, &wire.TxOut{Value: 42}, false, 1))
	state := *chain.stateSnapshot
	state.Hash[0] ^= 1
	cache.lastFlushTime = time.Now().Add(-UtxoSyncInterval - time.Minute)
	require.NoError(t, cache.flushToDB(FlushIfNeeded, &state))
	require.Equal(t, state.Hash, cache.lastFlushHash)
	require.Empty(t, cache.dirtyOutpoints)
	require.Equal(t, 1, cache.cachedEntries.length())
}

// Periodic sync cost should follow the dirty set rather than the size of the
// retained read cache. Each iteration modifies one existing persisted output.
func BenchmarkUtxoCacheDirtySync(b *testing.B) {
	for _, count := range []int{1000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			chain, _, cleanup := utxoCacheTestChain(b.Name())
			b.Cleanup(cleanup)
			cache := chain.utxoCache
			cache.maxTotalMemoryUsage = 128 << 20
			outpoints := populateFetchEntries(b, chain.db, count)
			_, err := cache.fetchEntries(outpoints)
			require.NoError(b, err)
			out := &wire.TxOut{Value: 42, PkScript: getValidP2PKHScript()}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := cache.addTxOut(outpoints[0], out, false, 1); err != nil {
					b.Fatal(err)
				}
				if err := cache.flushToDB(FlushSync, chain.stateSnapshot); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
