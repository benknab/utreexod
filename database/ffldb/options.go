package ffldb

import "fmt"

// Options configures the metadata database caches. A zero value preserves the
// LevelDB defaults. Options can be passed as the optional third argument to
// database.Open and database.Create. They do not change the database format.
type Options struct {
	// BlockCacheCapacity is the read cache capacity in bytes.
	BlockCacheCapacity int

	// OpenFilesCacheCapacity is the number of open table files to cache.
	OpenFilesCacheCapacity int
}

func (o Options) validate() error {
	if o.BlockCacheCapacity < 0 || o.OpenFilesCacheCapacity < 0 {
		return fmt.Errorf("ffldb cache capacities must not be negative")
	}
	return nil
}
