package indexers

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"slices"
	"testing"

	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/database/ffldb"
	"github.com/utreexo/utreexod/wire"
)

// This test-only candidate changes read order, not the persisted format or
// per-address transaction order. Keep it separate from production until an
// experiment justifies the additional allocation and sort.
func connectAddrEntriesSorted(tx database.Tx, blockID uint32, locs []wire.TxLoc, data writeIndexData) error {
	keys := make([][addrKeySize]byte, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b [addrKeySize]byte) int { return bytes.Compare(a[:], b[:]) })
	bucket := tx.Metadata().Bucket(addrIndexKey)
	for _, key := range keys {
		for _, index := range data[key] {
			if err := dbPutAddrIndexEntry(bucket, key, blockID, locs[index]); err != nil {
				return err
			}
		}
	}
	return nil
}

// BenchmarkAddrReadOrder isolates point-read locality against a nonempty,
// deterministic on-disk ffldb fixture. Each operation rolls back its writes, so
// every mode sees identical values and address history levels. The OS page cache
// is warm; the 1 MiB read cache scales down the dataset/cache ratio. This is NOT
// a mainnet replay, full catch-up benchmark, or compaction-topology reproduction.
func BenchmarkAddrReadOrder(b *testing.B) {
	const addresses = 250000
	keyFor := func(i int) [addrKeySize]byte {
		var input [8]byte
		binary.LittleEndian.PutUint64(input[:], uint64(i))
		hash := sha256.Sum256(input[:])
		var key [addrKeySize]byte
		key[0] = byte(i % 5)
		copy(key[1:], hash[:20])
		return key
	}
	path := filepath.Join(b.TempDir(), "metadata")
	db, err := database.Create("ffldb", path, wire.MainNet)
	if err != nil {
		b.Fatal(err)
	}
	if err := db.Update(func(tx database.Tx) error {
		bucket, err := tx.Metadata().CreateBucket(addrIndexKey)
		if err != nil {
			return err
		}
		value := make([]byte, 7*txEntrySize)
		for n := 0; n < 7; n++ {
			putTxIndexEntry(value[n*txEntrySize:], 1, wire.TxLoc{TxStart: 81 + 100*n, TxLen: 100})
		}
		for i := 0; i < addresses; i++ {
			key := keyForLevel(keyFor(i), 0)
			if err := bucket.Put(key[:], value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	data := make(writeIndexData, 5000)
	for i := 0; i < 5000; i++ {
		data[keyFor(i*7919%addresses)] = []int{0}
	}
	locs := []wire.TxLoc{{TxStart: 81, TxLen: 100}}
	for _, cache := range []struct {
		name string
		size int
	}{{"1MiB-cache", 1 << 20}, {"64MiB-cache", 64 << 20}} {
		b.Run(cache.name, func(b *testing.B) {
			for _, mode := range []struct {
				name  string
				apply func(database.Tx, uint32, []wire.TxLoc, writeIndexData) error
			}{
				{"map-a", connectAddrEntries}, {"sorted-a", connectAddrEntriesSorted},
				{"sorted-b", connectAddrEntriesSorted}, {"map-b", connectAddrEntries},
			} {
				b.Run(mode.name, func(b *testing.B) {
					db, err := database.Open("ffldb", path, wire.MainNet, ffldb.Options{
						BlockCacheCapacity: cache.size, OpenFilesCacheCapacity: 4096,
					})
					if err != nil {
						b.Fatal(err)
					}
					defer db.Close()
					run := func() {
						tx, err := db.Begin(true)
						if err != nil {
							b.Fatal(err)
						}
						if err := mode.apply(tx, 2, locs, data); err != nil {
							_ = tx.Rollback()
							b.Fatal(err)
						}
						if err := tx.Rollback(); err != nil {
							b.Fatal(err)
						}
					}
					for i := 0; i < 10; i++ {
						run() // Warm the same fixed trace before timing either mode.
					}
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						run()
					}
					b.StopTimer()
				})
			}
		})
	}
}
