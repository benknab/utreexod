package ffldb

import (
	"crypto/sha256"
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/utreexo/utreexod/database/internal/treap"
)

// BenchmarkIBDMetadataFlush writes the same UTXO-sized, sorted key/value set
// to a fresh database each iteration. Keep both the atomic commit and closing
// the database in the timed section, including file syncs and worker shutdown.
func BenchmarkIBDMetadataFlush(b *testing.B) {
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
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		idb, err := openDB(filepath.Join(b.TempDir(), "db"), blockDataNet, true)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		err = idb.(*db).cache.commitTreaps(keys, removes)
		closeErr := idb.Close()
		if err != nil {
			b.Fatal(err)
		}
		if closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}
