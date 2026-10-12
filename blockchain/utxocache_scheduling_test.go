package blockchain

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/wire"
)

// Model unequal read latency without evicting the host's live database cache.
// Data and snapshot semantics still come from the real FFLDB implementation.
type unevenReadDB struct {
	database.DB
	slow map[string]bool
}

type unevenReadTx struct {
	database.Tx
	slow map[string]bool
}

func (tx unevenReadTx) SupportsConcurrentReads() bool { return true }
func (tx unevenReadTx) Metadata() database.Bucket {
	return unevenReadBucket{readBucket{tx.Tx.Metadata()}, tx.slow}
}

type unevenReadBucket struct {
	readBucket
	slow map[string]bool
}

func (b unevenReadBucket) Bucket(key []byte) database.Bucket {
	return unevenReadBucket{readBucket{b.readBucket.Bucket.Bucket(key)}, b.slow}
}

func (b unevenReadBucket) Get(key []byte) []byte {
	if b.slow[string(key)] {
		time.Sleep(50 * time.Microsecond)
	}
	return b.readBucket.Get(key)
}

func (db unevenReadDB) View(fn func(database.Tx) error) error {
	return db.DB.View(func(tx database.Tx) error { return fn(unevenReadTx{tx, db.slow}) })
}

func BenchmarkUtxoCacheUnevenFetch(b *testing.B) {
	db, err := database.Create("ffldb", filepath.Join(b.TempDir(), "db"), wire.MainNet)
	require.NoError(b, err)
	b.Cleanup(func() { db.Close() })
	outpoints := populateFetchEntries(b, db, 4096)
	require.NoError(b, db.Flush())
	slow := make(map[string]bool)
	for i := 0; i < len(outpoints)/2; i += 32 {
		key := OutpointKey(outpoints[i])
		slow[string(*key)] = true
		RecycleOutpointKey(key)
	}
	wrapped := unevenReadDB{db, slow}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		cache := newUtxoCache(wrapped, 16<<20)
		entries, err := cache.fetchEntries(outpoints)
		if err != nil || len(entries) != len(outpoints) {
			b.Fatalf("fetch: %v", err)
		}
	}
}
