package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/btcsuite/btclog"
	flags "github.com/jessevdk/go-flags"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/database/ffldb"
)

func TestLevelDBConfig(t *testing.T) {
	t.Parallel()
	for _, source := range []string{"flags", "config file"} {
		t.Run(source, func(t *testing.T) {
			c := config{DbType: "ffldb"}
			parser := flags.NewParser(&c, flags.None)
			if source == "flags" {
				_, err := parser.ParseArgs([]string{"--leveldbcachemaxsize=1024", "--leveldbopenfiles=4096"})
				require.NoError(t, err)
			} else {
				err := flags.NewIniParser(parser).Parse(strings.NewReader(
					"[Application Options]\nleveldbcachemaxsize=1024\nleveldbopenfiles=4096\n"))
				require.NoError(t, err)
			}
			options, err := c.levelDBOptions()
			require.NoError(t, err)
			require.Equal(t, ffldb.Options{
				BlockCacheCapacity: 1024 * 1024 * 1024, OpenFilesCacheCapacity: 4096,
			}, options)
		})
	}

	for _, dbType := range []string{"ffldb", "memdb"} {
		c := config{DbType: dbType}
		options, err := c.levelDBOptions()
		require.NoError(t, err)
		require.Equal(t, ffldb.Options{}, options)
	}

	maxMiB := int(^uint(0)>>1) / (1024 * 1024)
	for _, args := range [][]string{
		{"--leveldbcachemaxsize=-1"},
		{"--leveldbopenfiles=-1"},
		{"--leveldbcachemaxsize=" + strconv.Itoa(maxMiB+1)},
		{"--dbtype=memdb", "--leveldbcachemaxsize=1"},
		{"--dbtype=memdb", "--leveldbopenfiles=1"},
	} {
		c := config{DbType: "ffldb"}
		_, err := flags.NewParser(&c, flags.None).ParseArgs(args)
		require.NoError(t, err)
		_, err = c.levelDBOptions()
		require.Error(t, err, "args: %v", args)
	}
}

func TestLoadBlockDBCacheOptions(t *testing.T) {
	previous := cfg
	previousLog := btcdLog
	var logs bytes.Buffer
	btcdLog = btclog.Disabled
	database.UseLogger(btclog.NewBackend(&logs).Logger("TEST"))
	t.Cleanup(func() {
		cfg = previous
		btcdLog = previousLog
		database.UseLogger(bcdbLog)
	})
	cfg = &config{
		DbType: "ffldb", DataDir: t.TempDir(),
		LevelDBCacheSizeMiB: 1024, LevelDBOpenFiles: 4096,
	}
	db, err := loadBlockDB()
	require.NoError(t, err)
	require.Contains(t, logs.String(), "LevelDB metadata caches: read=1024 MiB, open files=4096")
	err = db.Update(func(tx database.Tx) error {
		return tx.Metadata().Put([]byte("cache-test"), []byte("saved"))
	})
	require.NoError(t, db.Close())
	require.NoError(t, err)

	// Reopen the same database with the default caches. No rebuild is needed.
	cfg.LevelDBCacheSizeMiB = 0
	cfg.LevelDBOpenFiles = 0
	logs.Reset()
	db, err = loadBlockDB()
	require.NoError(t, err)
	require.Contains(t, logs.String(), "LevelDB metadata caches: read=8 MiB")
	err = db.View(func(tx database.Tx) error {
		require.Equal(t, []byte("saved"), tx.Metadata().Get([]byte("cache-test")))
		return nil
	})
	require.NoError(t, db.Close())
	require.NoError(t, err)
}
