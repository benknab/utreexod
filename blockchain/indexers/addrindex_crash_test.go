package indexers

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/database"
)

const addrCrashExit = 77

type abruptAddrCommit struct {
	database.DB
	mode string
}

func (db abruptAddrCommit) Update(fn func(database.Tx) error) error {
	err := db.DB.Update(func(tx database.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if db.mode == "before-commit" {
			os.Exit(addrCrashExit)
		}
		if db.mode == "after-spill" {
			// Exceed ffldb's 100 MiB metadata cache in this same transaction.
			// The address entries and tip must survive together when the
			// oversized transaction spills through LevelDB table files.
			padding, err := tx.Metadata().CreateBucket([]byte("crash-padding"))
			if err != nil {
				return err
			}
			value := bytes.Repeat([]byte{42}, 1<<20)
			for i := 0; i < 110; i++ {
				if err := padding.Put([]byte(fmt.Sprintf("%03d", i)), value); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		// Deliberately skip all Go defers, worker shutdown and database Close.
		os.Exit(addrCrashExit)
	}
	return err
}

// This tests abrupt process death, not power loss or a crash during an arbitrary
// filesystem syscall. The reference history comes from validated fixture blocks.
func TestBackgroundAddrAbruptExit(t *testing.T) {
	if path := os.Getenv("UTREEXOD_ADDR_CRASH_DB"); path != "" {
		f := &addrWorkerFixture{t: t, path: path, params: chaincfg.RegressionNetParams}
		f.params.CoinbaseMaturity = 1
		f.open(false)
		f.worker.db = abruptAddrCommit{DB: f.db, mode: os.Getenv("UTREEXOD_ADDR_CRASH_MODE")}
		_, _, err := f.worker.step()
		t.Fatalf("crash injection did not exit: %v", err)
	}

	for _, mode := range []string{"before-commit", "after-cache-commit", "after-spill"} {
		t.Run(mode, func(t *testing.T) {
			f := newAddrWorkerFixture(t)
			b1, out := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
			b2, _ := f.add(b1, out)
			_, _, err := f.worker.step() // Persist a genesis-only address checkpoint.
			require.NoError(t, err)
			require.NoError(t, f.worker.Stop())
			require.NoError(t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
			require.NoError(t, f.db.Close())

			cmd := exec.Command(os.Args[0], "-test.run=^TestBackgroundAddrAbruptExit$")
			cmd.Env = append(os.Environ(), "UTREEXOD_ADDR_CRASH_DB="+f.path,
				"UTREEXOD_ADDR_CRASH_MODE="+mode)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "%s", output)
			require.Equal(t, addrCrashExit, exit.ExitCode(), "%s", output)

			f.open(false)
			status := f.status()
			switch mode {
			case "before-commit":
				require.Equal(t, int32(0), status.Height)
			case "after-spill":
				require.Equal(t, int32(1), status.Height)
			default:
				// ffldb can lose an unflushed cache commit. It may not retain
				// entries without their tip, or the tip without its entries.
				require.Contains(t, []int32{0, 1}, status.Height)
			}
			key, err := addrToKey(f.addr)
			require.NoError(t, err)
			require.NoError(t, f.db.View(func(tx database.Tx) error {
				regions, _, err := dbFetchAddrIndexEntries(tx, key, 0, 100, false)
				require.NoError(t, err)
				require.Len(t, regions, int(status.Height))
				return nil
			}))
			f.catchUp()
			require.NoError(t, f.db.View(func(tx database.Tx) error {
				history := f.history(tx)
				require.Len(t, history, 3)
				for i, block := range []*btcutil.Block{b1, b2, b2} {
					require.Equal(t, *block.Hash(), *history[i].Hash)
				}
				page, skipped, err := f.worker.TxRegionsForAddress(tx, f.addr, 1, 1, true)
				require.NoError(t, err)
				require.Equal(t, uint32(1), skipped)
				require.Equal(t, history[1:2], page)
				return nil
			}))
			f.restart()
			require.True(t, f.status().Synced)
		})
	}
}
