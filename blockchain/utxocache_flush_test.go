package blockchain

import (
	"errors"
	"testing"

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
	require.ErrorIs(t, chain.FlushUtxoCache(FlushRequired), failure)
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
	require.NoError(t, chain.FlushUtxoCache(FlushRequired))
	require.Zero(t, cache.totalMemoryUsage())
	require.NoError(t, db.View(func(tx database.Tx) error {
		entry, err := dbFetchUtxoEntry(tx, tx.Metadata().Bucket(utxoSetBucketName), op)
		require.NotNil(t, entry)
		require.Equal(t, int64(42), entry.Amount())
		return err
	}))
}
