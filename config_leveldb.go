package main

import (
	"fmt"

	"github.com/utreexo/utreexod/database/ffldb"
)

// levelDBOptions validates the launch settings before converting MiB to bytes.
func (c *config) levelDBOptions() (ffldb.Options, error) {
	const mib = 1024 * 1024
	maxMiB := int(^uint(0)>>1) / mib
	if c.LevelDBCacheSizeMiB < 0 || c.LevelDBCacheSizeMiB > maxMiB {
		return ffldb.Options{}, fmt.Errorf("leveldbcachemaxsize must be between 0 and %d MiB", maxMiB)
	}
	if c.LevelDBOpenFiles < 0 {
		return ffldb.Options{}, fmt.Errorf("leveldbopenfiles must not be negative")
	}
	if c.DbType != "ffldb" && (c.LevelDBCacheSizeMiB != 0 || c.LevelDBOpenFiles != 0) {
		return ffldb.Options{}, fmt.Errorf("leveldbcachemaxsize and leveldbopenfiles require --dbtype=ffldb")
	}
	return ffldb.Options{
		BlockCacheCapacity:     c.LevelDBCacheSizeMiB * mib,
		OpenFilesCacheCapacity: c.LevelDBOpenFiles,
	}, nil
}
