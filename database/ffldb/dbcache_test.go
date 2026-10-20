package ffldb

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/database/internal/treap"
)

// A large UTXO flush must bypass the immutable cache instead of duplicating
// the transaction's treap and allocating whole-transaction KV slices.
func TestLargeTransactionBypassesCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	idb, err := openDB(path, blockDataNet, true, Options{})
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
	idb, err = openDB(path, blockDataNet, false, Options{})
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

// A flush larger than the LevelDB write buffer spills to table files before
// commit. All of those files must remain invisible on abort and appear together
// on commit, while readers keep their original snapshot.
func TestMetadataSpillTransactionAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	idb, err := openDB(path, blockDataNet, true, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { idb.Close() })
	d := idb.(*db)
	key := []byte("atomicity-existing")
	require.NoError(t, d.cache.ldb.Put(key, []byte("old"), nil))
	snapshot, err := d.cache.ldb.GetSnapshot()
	require.NoError(t, err)
	defer snapshot.Release()
	abort := errors.New("abort after spilling")
	value := bytes.Repeat([]byte{42}, 32*1024)
	write := func(tx *leveldb.Transaction, fail bool) error {
		if err := tx.Put(key, []byte("new"), nil); err != nil {
			return err
		}
		for i := 0; i < 1100; i++ {
			if err := tx.Put([]byte(fmt.Sprintf("atomicity-%04d", i)), value, nil); err != nil {
				return err
			}
		}
		if fail {
			return abort
		}
		return nil
	}
	require.ErrorIs(t, d.cache.updateDB(func(tx *leveldb.Transaction) error {
		return write(tx, true)
	}), abort)
	got, err := d.cache.ldb.Get(key, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("old"), got)
	for _, index := range []int{0, 1099} {
		_, err := d.cache.ldb.Get([]byte(fmt.Sprintf("atomicity-%04d", index)), nil)
		require.ErrorIs(t, err, leveldb.ErrNotFound)
	}
	require.NoError(t, d.cache.updateDB(func(tx *leveldb.Transaction) error {
		return write(tx, false)
	}))
	got, err = snapshot.Get(key, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("old"), got)
	snapshot.Release()
	require.NoError(t, idb.Close())
	idb, err = openDB(path, blockDataNet, false, Options{})
	require.NoError(t, err)
	d = idb.(*db)
	got, err = d.cache.ldb.Get(key, nil)
	require.NoError(t, err)
	require.Equal(t, []byte("new"), got)
	for _, index := range []int{0, 1099} {
		got, err := d.cache.ldb.Get([]byte(fmt.Sprintf("atomicity-%04d", index)), nil)
		require.NoError(t, err)
		require.Equal(t, value, got)
	}
}
