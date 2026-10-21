package indexers

import (
	"errors"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/txscript"
)

// pruningDB schedules a deterministic pruning boundary while retaining the real
// FFldb transaction and flush locks. This avoids generating 128 MiB of blocks.
type pruningDB struct {
	database.DB
	inUpdate   bool
	keepHeight int32
	attempts   int
}

var pruneMarker = []byte("prune-test-marker")

func (db *pruningDB) Update(fn func(database.Tx) error) error {
	return db.DB.Update(func(tx database.Tx) error {
		db.inUpdate = true
		defer func() { db.inUpdate = false }()
		return fn(&pruningTx{Tx: tx, db: db})
	})
}

type pruningTx struct {
	database.Tx
	db *pruningDB
}

func (tx *pruningTx) PruneBlocks(_ uint64, _ int32) (int32, error) {
	if tx.db.keepHeight == 0 {
		return -1, nil
	}
	tx.db.attempts++
	if err := tx.Metadata().Put(pruneMarker, []byte{1}); err != nil {
		return 0, err
	}
	return tx.db.keepHeight, nil
}

// TestPruneFlushOutsideTransaction covers both bridge indexes, using FFldb's
// actual Flush. An injected failure checks that pruning is rolled back rather
// than deleting recovery data when the required flush fails.
func TestPruneFlushOutsideTransaction(t *testing.T) {
	modes := []struct {
		name     string
		flat     bool
		pipeline bool
	}{
		{name: "database"},
		{name: "flat", flat: true},
		{name: "flat_archive_pipeline", flat: true, pipeline: true},
	}
	for _, mode := range modes {
		for _, failFlush := range []bool{false, true} {
			name := mode.name
			if failFlush {
				name += "/flush_failure"
			} else {
				name += "/success"
			}
			t.Run(name, func(t *testing.T) {
				params := chaincfg.RegressionNetParams
				dbPath := t.TempDir()
				base, err := database.Create(testDbType, dbPath, params.Net)
				require.NoError(t, err)
				t.Cleanup(func() { base.Close() })
				db := &pruningDB{DB: base}
				flushFailure := errors.New("injected pre-prune flush failure")
				flushCalls := 0
				flushMainDB := func() error {
					// Fail instead of hanging forever on the unfixed implementation.
					if db.inUpdate {
						return errors.New("main database flush inside transaction")
					}
					if db.keepHeight != 0 {
						flushCalls++
						err := base.View(func(tx database.Tx) error {
							require.Nil(t, tx.Metadata().Get(pruneMarker),
								"pending pruning must roll back before flushing")
							return nil
						})
						if err != nil {
							return err
						}
						if failFlush {
							return flushFailure
						}
					}
					return base.Flush()
				}

				var idx Indexer
				var flatIdx *FlatUtreexoProofIndex
				if mode.flat {
					flatIdx, err = NewFlatUtreexoProofIndex(!mode.pipeline, &params,
						50*1024*1024, dbPath, flushMainDB)
					require.NoError(t, err)
					idx = flatIdx
					t.Cleanup(func() {
						if mode.pipeline {
							flatIdx.StopProofPipeline()
						}
						flatIdx.CloseUtreexoState()
					})
				} else {
					dbIdx, err := NewUtreexoProofIndex(db, true,
						50*1024*1024, &params, dbPath, flushMainDB)
					require.NoError(t, err)
					idx = dbIdx
					t.Cleanup(func() { dbIdx.CloseUtreexoState() })
				}
				chain, err := blockchain.New(&blockchain.Config{
					DB: db, ChainParams: &params,
					TimeSource:       blockchain.NewMedianTime(),
					SigCache:         txscript.NewSigCache(1000),
					UtxoCacheMaxSize: 10 * 1024 * 1024,
					IndexManager:     NewManager(db, []Indexer{idx}),
					Prune:            128 * 1024 * 1024,
				})
				require.NoError(t, err)
				if mode.pipeline {
					require.NoError(t, flatIdx.StartProofPipeline())
				}
				prev := btcutil.NewBlock(params.GenesisBlock)
				prev.SetHeight(0)
				first, _ := blockchain.NewBlock(chain, prev, nil)
				_, _, err = chain.ProcessBlock(first, blockchain.BFNone)
				require.NoError(t, err)
				second, _ := blockchain.NewBlock(chain, first, nil)
				_, _, err = chain.ProcessBlock(second, blockchain.BFNone)
				require.NoError(t, err)
				third, _ := blockchain.NewBlock(chain, second, nil)

				// The persisted accumulator is at genesis, so dropping genesis
				// requires a flush of the still-connected second block.
				db.keepHeight = 1
				_, _, err = chain.ProcessBlock(third, blockchain.BFNone)
				require.Equal(t, 1, flushCalls)
				if failFlush {
					require.ErrorIs(t, err, flushFailure)
					require.Equal(t, 1, db.attempts)
					require.Equal(t, *second.Hash(), chain.BestSnapshot().Hash)
				} else {
					require.NoError(t, err)
					require.Equal(t, 2, db.attempts,
						"pruning must retry after the out-of-transaction flush")
					require.Equal(t, *third.Hash(), chain.BestSnapshot().Hash)
				}
				err = base.View(func(tx database.Tx) error {
					if failFlush {
						require.Nil(t, tx.Metadata().Get(pruneMarker))
					} else {
						require.Equal(t, []byte{1}, tx.Metadata().Get(pruneMarker))
					}
					return nil
				})
				require.NoError(t, err)
			})
		}
	}
}
