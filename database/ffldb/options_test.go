package ffldb

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
)

func TestCacheOptionsReopen(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "db")
	// Create with the old two-argument API, then change cache settings twice.
	db, err := database.Create(dbType, path, blockDataNet)
	require.NoError(t, err)
	require.NoError(t, db.Update(func(tx database.Tx) error {
		return tx.Metadata().Put([]byte("progress"), []byte("saved"))
	}))
	require.NoError(t, db.Close())

	for _, options := range []Options{
		{BlockCacheCapacity: 1024 * 1024 * 1024, OpenFilesCacheCapacity: 4096},
		{},
	} {
		db, err := database.Open(dbType, path, blockDataNet, options)
		require.NoError(t, err)
		err = db.View(func(tx database.Tx) error {
			require.Equal(t, []byte("saved"), tx.Metadata().Get([]byte("progress")))
			return nil
		})
		require.NoError(t, db.Close())
		require.NoError(t, err)
	}
}

func TestCacheOptionsInvalid(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		options interface{}
	}{
		{"wrong type", 1},
		{"nil", nil},
		{"negative read cache", Options{BlockCacheCapacity: -1}},
		{"negative file cache", Options{OpenFilesCacheCapacity: -1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			_, err := database.Create(dbType, path, blockDataNet, test.options)
			require.Error(t, err)
			require.NoDirExists(t, path)
			_, err = database.Open(dbType, path, blockDataNet, test.options)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "does not exist")
		})
	}
}
