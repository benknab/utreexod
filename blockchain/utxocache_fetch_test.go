package blockchain

import (
	"bytes"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
)

// Hiding the optional capability exercises the fallback for other backends.
type serialReadTx struct{ database.Tx }
type serialReadDB struct{ database.DB }

func (db serialReadDB) View(fn func(database.Tx) error) error {
	return db.DB.View(func(tx database.Tx) error { return fn(serialReadTx{tx}) })
}

func populateFetchEntries(t testing.TB, db database.DB, count int) []wire.OutPoint {
	t.Helper()
	outpoints := make([]wire.OutPoint, count)
	require.NoError(t, db.Update(func(tx database.Tx) error {
		bucket, err := tx.Metadata().CreateBucketIfNotExists(utxoSetBucketName)
		if err != nil {
			return err
		}
		for i := range outpoints {
			outpoints[i] = outpointFromInt(i)
			entry := NewUtxoEntry(&wire.TxOut{Value: int64(i + 1), PkScript: getValidP2PKHScript()}, 1, i%2 == 0)
			if err := dbPutUtxoEntry(bucket, outpoints[i], entry); err != nil {
				return err
			}
		}
		return nil
	}))
	return outpoints
}

func TestUtxoCacheParallelFetch(t *testing.T) {
	old := runtime.GOMAXPROCS(4)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	outpoints := populateFetchEntries(t, chain.db, 1024)
	// Include missing, cached, and repeated outpoints in the same request.
	outpoints = append(outpoints, outpointFromInt(2000), outpoints[0], outpoints[500])
	serial := newUtxoCache(serialReadDB{chain.db}, 1<<20)
	parallel := newUtxoCache(chain.db, 1<<20)
	_, err := parallel.fetchEntries(outpoints[10:20])
	require.NoError(t, err)
	want, err := serial.fetchEntries(outpoints)
	require.NoError(t, err)
	got, err := parallel.fetchEntries(outpoints)
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Nil(t, got[1024])
	require.Same(t, got[0], got[1025])
	require.Same(t, got[500], got[1026])
	require.Equal(t, serial.totalMemoryUsage(), parallel.totalMemoryUsage())

	// Corruption in separate worker ranges must return the same first error
	// and leave the cache untouched, even if later ranges finish first.
	require.NoError(t, chain.db.Update(func(tx database.Tx) error {
		bucket := tx.Metadata().Bucket(utxoSetBucketName)
		for _, index := range []int{20, 900} {
			key := OutpointKey(outpoints[index])
			err := bucket.Put(bytes.Clone(*key), []byte{0xff})
			RecycleOutpointKey(key)
			if err != nil {
				return err
			}
		}
		return nil
	}))
	serial = newUtxoCache(serialReadDB{chain.db}, 1<<20)
	parallel = newUtxoCache(chain.db, 1<<20)
	_, wantErr := serial.fetchEntries(outpoints)
	_, gotErr := parallel.fetchEntries(outpoints)
	require.Error(t, wantErr)
	require.EqualError(t, gotErr, wantErr.Error())
	require.Zero(t, parallel.cachedEntries.length())
	require.Zero(t, parallel.totalEntryMemory)
}

type pausedReadDB struct {
	database.DB
	entered chan struct{}
	release chan struct{}
	active  atomic.Int32
}

type pausedReadTx struct {
	database.Tx
	db *pausedReadDB
}

func (tx pausedReadTx) SupportsConcurrentReads() bool { return true }
func (tx pausedReadTx) Metadata() database.Bucket {
	return pausedReadBucket{readBucket: readBucket{tx.Tx.Metadata()}, db: tx.db}
}

type readBucket struct{ database.Bucket }

type pausedReadBucket struct {
	readBucket
	db *pausedReadDB
}

func (b pausedReadBucket) Bucket(key []byte) database.Bucket {
	return pausedReadBucket{readBucket: readBucket{b.readBucket.Bucket.Bucket(key)}, db: b.db}
}

func (b pausedReadBucket) Get(key []byte) []byte {
	b.db.active.Add(1)
	defer b.db.active.Add(-1)
	select {
	case b.db.entered <- struct{}{}:
	default:
	}
	<-b.db.release
	return b.readBucket.Get(key)
}

func (db *pausedReadDB) View(fn func(database.Tx) error) error {
	return db.DB.View(func(tx database.Tx) error { return fn(pausedReadTx{tx, db}) })
}

func TestUtxoCacheParallelSnapshot(t *testing.T) {
	old := runtime.GOMAXPROCS(4)
	t.Cleanup(func() { runtime.GOMAXPROCS(old) })
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	outpoints := populateFetchEntries(t, chain.db, 1024)
	db := &pausedReadDB{DB: chain.db, entered: make(chan struct{}, 4), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(db.release) }) }
	defer release()
	cache := newUtxoCache(db, 1<<20)
	var entries []*UtxoEntry
	var fetchErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		entries, fetchErr = cache.fetchEntries(outpoints)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-db.entered:
		case <-time.After(10 * time.Second):
			release()
			<-done
			t.Fatal("point reads did not overlap")
		}
	}
	// A newer commit while all readers are paused must not change their
	// snapshot. The next transaction must see the newer version instead.
	require.NoError(t, chain.db.Update(func(tx database.Tx) error {
		bucket := tx.Metadata().Bucket(utxoSetBucketName)
		for _, op := range outpoints {
			if err := dbPutUtxoEntry(bucket, op, NewUtxoEntry(&wire.TxOut{Value: 9999}, 2, false)); err != nil {
				return err
			}
		}
		return nil
	}))
	release()
	<-done
	require.NoError(t, fetchErr)
	require.Zero(t, db.active.Load(), "all readers must finish before View returns")
	for i, entry := range entries {
		require.Equal(t, int64(i+1), entry.Amount())
	}
	next := newUtxoCache(chain.db, 1<<20)
	entries, err := next.fetchEntries(outpoints)
	require.NoError(t, err)
	for _, entry := range entries {
		require.Equal(t, int64(9999), entry.Amount())
	}
}

func BenchmarkUtxoCacheFetch(b *testing.B) {
	db, err := database.Create("ffldb", filepath.Join(b.TempDir(), "db"), wire.MainNet)
	require.NoError(b, err)
	b.Cleanup(func() { db.Close() })
	outpoints := populateFetchEntries(b, db, 16384)
	require.NoError(b, db.Flush())
	for _, tc := range []struct {
		name string
		db   database.DB
	}{
		{"serial", serialReadDB{db}},
		{"parallel", db},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				cache := newUtxoCache(tc.db, 16<<20)
				entries, err := cache.fetchEntries(outpoints)
				if err != nil {
					b.Fatal(err)
				}
				if len(entries) != len(outpoints) || entries[len(entries)-1] == nil {
					b.Fatal("incomplete prevout fetch")
				}
			}
		})
	}
}

func TestFetchUtxoEntryDetached(t *testing.T) {
	chain, _, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	outpoints := populateFetchEntries(t, chain.db, 16)
	var wg sync.WaitGroup
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, op := range outpoints {
				entry, err := chain.FetchUtxoEntry(op)
				if err != nil || entry == nil {
					t.Errorf("fetch %v: %v", op, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	entry, err := chain.FetchUtxoEntry(outpoints[0])
	require.NoError(t, err)
	entry.Spend()
	again, err := chain.FetchUtxoEntry(outpoints[0])
	require.NoError(t, err)
	require.False(t, again.IsSpent(), "callers must not mutate the internal cache")
}
