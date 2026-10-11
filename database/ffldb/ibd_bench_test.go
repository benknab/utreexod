package ffldb

import (
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/utreexo/utreexod/database/internal/treap"
)

// BenchmarkIBDMetadataFlush writes the same UTXO-sized, sorted key/value set
// to a fresh database each iteration. Keep both the atomic commit and closing
// the database in the timed section, including file syncs and worker shutdown.
func BenchmarkIBDMetadataFlush(b *testing.B) {
	benchmarkIBDMetadataFlush(b, 1)
}

// Include repeated, overlapping updates and wait for full-range compaction so
// the timing includes merging the obsolete records created by earlier flushes.
func BenchmarkIBDMetadataRewrite(b *testing.B) {
	benchmarkIBDMetadataFlush(b, 4)
}

func benchmarkIBDMetadataFlush(b *testing.B, flushes int) {
	keys := treap.NewMutable()
	removes := treap.NewMutable()
	var input [8]byte
	for i := uint64(0); i < 500000; i++ {
		binary.LittleEndian.PutUint64(input[:], i)
		hash := sha256.Sum256(input[:])
		key := make([]byte, 40)
		copy(key[4:], hash[:])
		value := make([]byte, 64)
		copy(value, hash[:])
		keys.Put(key, value)
	}
	for _, tc := range []struct {
		name               string
		table, writeBuffer int
	}{
		{"2MiB-table_4MiB-buffer", 2 << 20, 4 << 20},
		{"32MiB-table_4MiB-buffer", 32 << 20, 4 << 20},
		{"32MiB-table_32MiB-buffer", 32 << 20, 32 << 20},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(flushes) * 500000 * 104)
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				path := filepath.Join(b.TempDir(), "db")
				ldb, err := leveldb.OpenFile(path, &opt.Options{
					Compression: opt.NoCompression, Filter: filter.NewBloomFilter(10),
					CompactionTableSize: tc.table, WriteBuffer: tc.writeBuffer,
				})
				if err != nil {
					b.Fatal(err)
				}
				cache := &dbCache{ldb: ldb}
				b.StartTimer()
				for flush := 0; flush < flushes && err == nil; flush++ {
					err = cache.commitTreaps(keys, removes)
				}
				if err == nil && flushes > 1 {
					err = ldb.CompactRange(util.Range{})
				}
				closeErr := ldb.Close()
				b.StopTimer()
				if err != nil {
					b.Fatal(err)
				}
				if closeErr != nil {
					b.Fatal(closeErr)
				}
				files, err := os.ReadDir(path)
				if err != nil {
					b.Fatal(err)
				}
				tables := 0
				for _, file := range files {
					if filepath.Ext(file.Name()) == ".ldb" {
						tables++
					}
				}
				b.ReportMetric(float64(tables), "tables")
			}
		})
	}
}
