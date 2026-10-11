package indexers

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/wire"
)

func ttlBatchFixture(t testing.TB, heights int) (*FlatFileState, []wire.LeafData, []int32) {
	t.Helper()
	f := NewFlatFileState()
	if err := f.Init(t.TempDir(), "ttl"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.dataFile.Close(); f.offsetFile.Close() })
	for h := 1; h <= heights; h++ {
		if err := f.StoreData(int32(h), make([]byte, 5000*ttlSize)); err != nil {
			t.Fatal(err)
		}
	}
	lds, indexes := make([]wire.LeafData, 5000), make([]int32, 5000)
	for i := range lds {
		lds[i].Height = int32(i%heights + 1)
		indexes[i] = int32(i)
	}
	return f, lds, indexes
}

func TestTTLBatchHeaderCache(t *testing.T) {
	f, lds, indexes := ttlBatchFixture(t, 7)
	require.NoError(t, writeTTLs(100, indexes, lds, f))
	for h := int32(1); h <= 7; h++ {
		data, err := f.FetchData(h)
		require.NoError(t, err)
		for i := range lds {
			if lds[i].Height != h {
				continue
			}
			require.Equal(t, uint32(100), byteOrder.Uint32(data[i*ttlSize:]))
			require.Equal(t, uint32(i), byteOrder.Uint32(data[i*ttlSize+4:]))
		}
	}
	// Repeated heights still validate every payload bound.
	indexes[7] = 5000
	require.ErrorContains(t, writeTTLs(101, indexes, lds, f), "overwrite outside")
	indexes[7] = 7
	// The cache cannot survive an operation: a subsequent corrupt header must fail.
	_, err := f.dataFile.WriteAt([]byte{0}, f.offsets[1])
	require.NoError(t, err)
	require.ErrorContains(t, writeTTLs(101, indexes, lds, f), "wrong magic")
}

func TestTTLCoalescingPreservesOverwriteOrder(t *testing.T) {
	f, lds, indexes := ttlBatchFixture(t, 2)
	want := [][]byte{make([]byte, 5000*ttlSize), make([]byte, 5000*ttlSize)}
	for i := range lds {
		h := (i / 1000) % 2
		lds[i].Height = int32(h + 1)
		indexes[i] = int32(i % 1000)
		offset := int(indexes[i]) * ttlSize
		byteOrder.PutUint32(want[h][offset:], 100)
		byteOrder.PutUint32(want[h][offset+4:], uint32(i))
	}
	require.NoError(t, writeTTLs(100, indexes, lds, f))
	for h := range want {
		got, err := f.FetchData(int32(h + 1))
		require.NoError(t, err)
		require.Equal(t, want[h], got)
	}
	indexes[0] = 1 << 29 // previously wrapped the int32 byte offset to zero
	require.ErrorContains(t, writeTTLs(100, indexes, lds, f), "overwrite outside")
}

func BenchmarkTTLBatchHeaders(b *testing.B) {
	for _, heights := range []int{1, 1000, 5000} {
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("heights=%d/cached=%t", heights, cached), func(b *testing.B) {
				f, lds, indexes := ttlBatchFixture(b, heights)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if cached {
						if err := writeTTLs(10000, indexes, lds, f); err != nil {
							b.Fatal(err)
						}
					} else {
						f.mtx.Lock()
						var buf [ttlSize]byte
						for i, ld := range lds {
							byteOrder.PutUint32(buf[:4], 10000)
							byteOrder.PutUint32(buf[4:], uint32(i))
							if err := f.overwriteLocked(ld.Height, indexes[i]*ttlSize, buf[:]); err != nil {
								b.Fatal(err)
							}
						}
						f.mtx.Unlock()
					}
				}
			})
		}
	}
}
