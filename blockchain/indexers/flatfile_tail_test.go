package indexers

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// StoreData can lose its offset publication after writing a complete or
// partial data record. Reopening must discard that tail before append/undo.
func TestRecoverUnpublishedFlatTail(t *testing.T) {
	for _, offsetTail := range []bool{false, true} {
		t.Run(map[bool]string{false: "data-only", true: "partial-offset"}[offsetTail], func(t *testing.T) {
			path := t.TempDir()
			ff := NewFlatFileState()
			require.NoError(t, ff.Init(path, "tail"))
			require.NoError(t, ff.StoreData(1, []byte("first")))
			require.NoError(t, ff.StoreData(2, []byte("second")))
			end := ff.currentOffset
			_, err := ff.dataFile.WriteAt([]byte{0xaa, 0xff, 0xaa}, end)
			require.NoError(t, err)
			if offsetTail {
				_, err = ff.offsetFile.WriteAt([]byte{0, 0, 0}, 24)
				require.NoError(t, err)
			}
			require.NoError(t, ff.dataFile.Close())
			require.NoError(t, ff.offsetFile.Close())
			ff = NewFlatFileState()
			require.NoError(t, ff.Init(path, "tail"))
			defer ff.dataFile.Close()
			defer ff.offsetFile.Close()
			require.Equal(t, end, ff.currentOffset)
			require.NoError(t, ff.DisconnectBlock(2))
			require.NoError(t, ff.StoreData(2, []byte("replacement")))
			data, err := ff.FetchData(1)
			require.NoError(t, err)
			require.Equal(t, []byte("first"), data)
			data, err = ff.FetchData(2)
			require.NoError(t, err)
			require.Equal(t, []byte("replacement"), data)
		})
	}
}
