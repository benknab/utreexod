package ffldb

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/database/internal/treap"
)

// A large UTXO flush must bypass the immutable cache instead of duplicating
// the transaction's treap and allocating whole-transaction KV slices.
func TestLargeTransactionBypassesCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	idb, err := openDB(path, blockDataNet, true)
	require.NoError(t, err)
	t.Cleanup(func() { idb.Close() })
	d := idb.(*db)
	d.cache.maxSize = 1024
	require.NoError(t, idb.Update(func(tx database.Tx) error {
		return tx.Metadata().Put([]byte("existing"), []byte("old"))
	}))
	before, err := idb.Begin(false)
	require.NoError(t, err)
	defer before.Rollback()
	value := bytes.Repeat([]byte{42}, 2048)
	require.NoError(t, idb.Update(func(tx database.Tx) error {
		if err := tx.Metadata().Put([]byte("existing"), []byte("new")); err != nil {
			return err
		}
		return tx.Metadata().Put([]byte("large"), value)
	}))
	require.Zero(t, d.cache.cachedKeys.Len(), "oversized transaction was duplicated into the shared cache")
	require.Zero(t, d.cache.cachedRemove.Len())
	require.Equal(t, []byte("old"), before.Metadata().Get([]byte("existing")))
	require.Nil(t, before.Metadata().Get([]byte("large")))
	require.NoError(t, before.Rollback())
	check := func(tx database.Tx) error {
		require.Equal(t, []byte("new"), tx.Metadata().Get([]byte("existing")))
		require.Equal(t, value, tx.Metadata().Get([]byte("large")))
		return nil
	}
	require.NoError(t, idb.View(check))
	require.NoError(t, idb.Close())
	idb, err = openDB(path, blockDataNet, false)
	require.NoError(t, err)
	require.NoError(t, idb.View(check))
}

func TestCacheBudgetIncludesPendingDeletes(t *testing.T) {
	c := &dbCache{maxSize: 1024, lastFlush: time.Now(), flushInterval: time.Hour}
	tx := &transaction{
		snapshot:    &dbCacheSnapshot{pendingKeys: treap.NewImmutable(), pendingRemove: treap.NewImmutable()},
		pendingKeys: treap.NewMutable(), pendingRemove: treap.NewMutable(),
	}
	require.False(t, c.needsFlush(tx))
	for i := byte(0); i < 32; i++ {
		tx.pendingRemove.Put(bytes.Repeat([]byte{i}, 64), nil)
	}
	require.True(t, c.needsFlush(tx), "pending deletes also consume transaction memory")
}
