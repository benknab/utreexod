package blockchain

import (
	"fmt"
	"testing"

	"github.com/utreexo/utreexod/wire"
)

func BenchmarkUtxoCacheSpendHit(b *testing.B) {
	cache := newUtxoCache(nil, 1<<20)
	in := &wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}}
	entry := &UtxoEntry{packedFlags: tfModified}
	cache.cachedEntries.put(in.PreviousOutPoint, entry)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		entry.packedFlags = tfModified
		if err := cache.addTxIn(in, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUtxoCacheFetchHits(b *testing.B) {
	for _, count := range []int{1, 5000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			cache := newUtxoCache(nil, 16<<20)
			outpoints := make([]wire.OutPoint, count)
			for i := range outpoints {
				outpoints[i].Index = uint32(i)
				cache.cachedEntries.put(outpoints[i], &UtxoEntry{})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := cache.fetchEntries(outpoints); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestUtxoCacheCachedMiss(t *testing.T) {
	// A nil DB makes any accidental refetch of the known miss fail.
	cache := newUtxoCache(nil, 1<<20)
	missing := wire.OutPoint{Index: 1}
	present := wire.OutPoint{Index: 2}
	entry := &UtxoEntry{amount: 42}
	cache.cachedEntries.put(missing, nil)
	cache.cachedEntries.put(present, entry)
	chain := &BlockChain{utxoCache: cache}
	if got, err := chain.FetchUtxoEntry(missing); got != nil || err != nil {
		t.Fatalf("cached miss: %v, %v", got, err)
	}
	entries, err := cache.fetchEntries([]wire.OutPoint{missing, present, missing, present})
	if err != nil || len(entries) != 4 || entries[0] != nil || entries[2] != nil ||
		entries[1] != entry || entries[3] != entry {
		t.Fatalf("cached results or aliasing changed: %v, %v", entries, err)
	}
}
