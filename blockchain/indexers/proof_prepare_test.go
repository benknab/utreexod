package indexers

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/database"
)

// Gate one metadata writer after recovery and the initial durable interval.
type gatedProofDB struct {
	database.DB
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
	err     error
	onFlush bool
}

func (db *gatedProofDB) Update(fn func(database.Tx) error) error {
	if !db.onFlush {
		if err := db.gate(); err != nil {
			return err
		}
	}
	return db.DB.Update(fn)
}

func (db *gatedProofDB) Flush() error {
	if db.onFlush {
		if err := db.gate(); err != nil {
			return err
		}
	}
	return db.DB.Flush()
}

func (db *gatedProofDB) gate() error {
	if db.armed.CompareAndSwap(true, false) {
		close(db.entered)
		<-db.release
		if db.err != nil {
			return db.err
		}
	}
	return nil
}

func TestProofIntentPreparationOverlap(t *testing.T) {
	for _, mode := range []string{"success", "failure", "flush-failure", "pause", "reorg", "stop"} {
		t.Run(mode, func(t *testing.T) {
			f := newProofWorkerFixture(t)
			tip := btcutil.NewBlock(f.params.GenesisBlock)
			for range 180 {
				tip, _ = f.add(tip, nil)
			}
			gate := &gatedProofDB{DB: f.db, entered: make(chan struct{}), release: make(chan struct{})}
			failed := mode == "failure" || mode == "flush-failure"
			gate.onFlush = mode == "flush-failure"
			if failed {
				gate.err = errors.New("injected intent writer failure")
			}
			var release sync.Once
			defer release.Do(func() { close(gate.release) })
			f.worker.db = gate
			f.worker.delay = time.Millisecond
			f.worker.SetPaused(false)
			f.start()
			require.Eventually(t, func() bool { return f.worker.Snapshot().Height >= 10 }, 10*time.Second, time.Millisecond)
			gate.armed.Store(true)
			select {
			case <-gate.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("next intent was not queued")
			}
			// The writer was queued with useful work still in the old interval.
			require.Less(t, f.worker.Snapshot().Height, int32(100))
			require.Eventually(t, func() bool { return f.worker.Snapshot().Height == 127 }, 10*time.Second, time.Millisecond)
			time.Sleep(20 * time.Millisecond)
			require.Equal(t, int32(127), f.worker.Snapshot().Height)
			var stopped chan error
			switch mode {
			case "pause":
				f.worker.SetPaused(true)
			case "reorg":
				// A shallow fork exercises pending-intent replacement without
				// making the test dominated by dozens of durable undo syncs.
				fork, err := f.chain.BlockByHeight(120)
				require.NoError(t, err)
				next, _ := blockchain.NewBlock(f.chain, fork, nil)
				next.MsgBlock().Header.Timestamp = next.MsgBlock().Header.Timestamp.Add(time.Second)
				require.True(t, blockchain.SolveBlock(&next.MsgBlock().Header))
				fork = btcutil.NewBlock(next.MsgBlock())
				fork.SetHeight(121)
				_, _, err = f.chain.ProcessBlock(fork, blockchain.BFNone)
				require.NoError(t, err)
				for range 69 {
					fork, _ = f.add(fork, nil)
				}
			case "stop":
				stopped = make(chan error, 1)
				go func() { stopped <- f.worker.Stop() }()
			}
			release.Do(func() { close(gate.release) })
			if failed {
				require.Eventually(t, func() bool { return f.worker.Snapshot().Stopped }, 10*time.Second, time.Millisecond)
				require.Contains(t, f.worker.Snapshot().LastError, "injected intent writer failure")
				require.Equal(t, int32(127), f.worker.Snapshot().Height)
				<-f.worker.done
				f.closed = true
				f.running = false
				f.openWorker()
				f.waitSynced()
				f.compare()
			} else {
				if mode == "pause" {
					require.Eventually(t, func() bool {
						cleared := false
						err := f.db.View(func(tx database.Tx) error {
							cleared = len(tx.Metadata().Bucket(f.worker.Key()).Get(proofReplayKey)) == 0
							return nil
						})
						return err == nil && cleared
					}, 10*time.Second, time.Millisecond)
				}
				if stopped != nil {
					require.NoError(t, <-stopped)
					f.closed = true
					f.running = false
					f.openWorker()
				}
				f.waitSynced()
				f.compare()
			}
		})
	}
}

// A fixed writer stall overlaps a fixed amount of proof work. This measures
// scheduling headroom, not mainnet throughput or actual disk-flush latency.
func BenchmarkProofIntentWriterStall(b *testing.B) {
	for range b.N {
		b.StopTimer()
		f := proofWorkerFixtureAt(b, b.TempDir(), true)
		tip := btcutil.NewBlock(f.params.GenesisBlock)
		for range 180 {
			tip, _ = f.add(tip, nil)
		}
		gate := &gatedProofDB{DB: f.db, entered: make(chan struct{}), release: make(chan struct{})}
		f.worker.db = gate
		f.worker.delay = 2 * time.Millisecond
		b.StartTimer()
		f.worker.SetPaused(false)
		f.start()
		require.Eventually(b, func() bool { return f.worker.Snapshot().Height >= 10 }, 10*time.Second, time.Millisecond)
		gate.armed.Store(true)
		go func() { <-gate.entered; time.Sleep(200 * time.Millisecond); close(gate.release) }()
		f.waitSynced()
		b.StopTimer()
		f.closeWorker()
	}
}
