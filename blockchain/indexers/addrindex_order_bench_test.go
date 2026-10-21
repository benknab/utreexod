package indexers

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/database/ffldb"
)

// Preserve the original map loop as the benchmark and differential-test oracle.
func connectAddrEntriesUnsorted(tx database.Tx, blockID uint32, locs []wire.TxLoc, data writeIndexData) error {
	bucket := tx.Metadata().Bucket(addrIndexKey)
	for key, indexes := range data {
		for _, index := range indexes {
			if err := dbPutAddrIndexEntry(bucket, key, blockID, locs[index]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check sort overhead where storage locality cannot help: empty/new addresses
// and reads served entirely by ffldb's dirty metadata cache.
func BenchmarkAddrReadOrderSmall(b *testing.B) {
	path := filepath.Join(b.TempDir(), "db")
	db, err := database.Create("ffldb", path, wire.MainNet)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	keyFor := func(i int) [addrKeySize]byte {
		var input [8]byte
		binary.LittleEndian.PutUint64(input[:], uint64(i))
		hash := sha256.Sum256(input[:])
		var key [addrKeySize]byte
		copy(key[1:], hash[:20])
		return key
	}
	locs := []wire.TxLoc{{TxStart: 81, TxLen: 100}}
	if err := db.Update(func(tx database.Tx) error {
		bucket, err := tx.Metadata().CreateBucket(addrIndexKey)
		if err != nil {
			return err
		}
		for i := 0; i < 5000; i++ {
			if err := dbPutAddrIndexEntry(bucket, keyFor(i), 1, locs[0]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		b.Fatal(err)
	}
	for _, count := range []int{0, 1, 8, 64, 5000} {
		for _, kind := range []string{"new", "dirty"} {
			data := make(writeIndexData)
			for i := 0; i < count; i++ {
				index := i
				if kind == "new" {
					index += 10000
				}
				data[keyFor(index)] = []int{0}
			}
			b.Run(fmt.Sprintf("%s-%d", kind, count), func(b *testing.B) {
				for _, mode := range []struct {
					name  string
					apply func(database.Tx, uint32, []wire.TxLoc, writeIndexData) error
				}{{"map", connectAddrEntriesUnsorted}, {"sorted", connectAddrEntries}} {
					b.Run(mode.name, func(b *testing.B) {
						b.ReportAllocs()
						for i := 0; i < b.N; i++ {
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
					})
				}
			})
		}
	}
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
				{"map-a", connectAddrEntriesUnsorted}, {"sorted-a", connectAddrEntries},
				{"sorted-b", connectAddrEntries}, {"map-b", connectAddrEntriesUnsorted},
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
