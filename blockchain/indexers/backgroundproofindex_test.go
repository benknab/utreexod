package indexers

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/database"
	"github.com/utreexo/utreexod/txscript"
	"github.com/utreexo/utreexod/wire"
)

type proofWorkerFixture struct {
	t       testing.TB
	db      database.DB
	path    string
	params  chaincfg.Params
	chain   *blockchain.BlockChain
	worker  *BackgroundProofIndex
	running bool
	closed  bool
}

func newProofWorkerFixture(t *testing.T) *proofWorkerFixture {
	return proofWorkerFixtureAt(t, t.TempDir(), true)
}

func proofWorkerFixtureAt(t testing.TB, path string, create bool) *proofWorkerFixture {
	f := &proofWorkerFixture{t: t, path: path, params: chaincfg.RegressionNetParams}
	f.params.CoinbaseMaturity = 1
	var err error
	if create {
		f.db, err = database.Create("ffldb", filepath.Join(f.path, "blocks"), wire.MainNet)
	} else {
		f.db, err = database.Open("ffldb", filepath.Join(f.path, "blocks"), wire.MainNet)
	}
	require.NoError(t, err)
	f.openWorker()
	f.chain, err = blockchain.New(&blockchain.Config{
		DB: f.db, ChainParams: &f.params, TimeSource: blockchain.NewMedianTime(),
		SigCache: txscript.NewSigCache(100), UtxoCacheMaxSize: 1 << 20,
		IndexManager: NewManager(f.db, []Indexer{NewTxIndex(f.db), f.worker}),
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		f.closeWorker()
		require.NoError(t, f.db.Close())
	})
	return f
}

func (f *proofWorkerFixture) openWorker() {
	f.closed = false
	idx, err := NewFlatUtreexoProofIndex(false, &f.params, 1<<20,
		filepath.Join(f.path, "proofs"), f.db.Flush)
	require.NoError(f.t, err)
	f.worker, err = NewBackgroundProofIndex(f.db, idx, 0, true)
	require.NoError(f.t, err)
	if f.chain != nil {
		require.NoError(f.t, NewManager(f.db, []Indexer{f.worker}).Init(f.chain, nil))
	}
}

func (f *proofWorkerFixture) closeWorker() {
	if f.closed {
		return
	}
	f.closed = true
	if f.running {
		require.NoError(f.t, f.worker.Stop())
		f.running = false
		return
	}
	if f.worker.utreexoState != nil {
		require.NoError(f.t, f.worker.utreexoState.state.CloseWithoutFlush())
	}
	for _, state := range f.worker.flatStates() {
		require.NoError(f.t, state.dataFile.Close())
		require.NoError(f.t, state.offsetFile.Close())
	}
}

func (f *proofWorkerFixture) start() {
	f.worker.Start()
	f.running = true
}

func (f *proofWorkerFixture) waitSynced() {
	f.worker.SetPaused(false)
	f.start()
	require.Eventually(f.t, func() bool {
		s := f.worker.Snapshot()
		return s.Synced || s.Stopped
	}, 20*time.Second, 5*time.Millisecond)
	require.Empty(f.t, f.worker.Snapshot().LastError)
	require.True(f.t, f.worker.Snapshot().Synced)
	f.worker.SetPaused(true)
	// All test inspection below holds the same gate as serving code.
	f.worker.access.Lock()
	f.worker.access.Unlock()
}

func (f *proofWorkerFixture) reopenDatabase() {
	require.NoError(f.t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
	require.NoError(f.t, f.db.Close())
	var err error
	f.db, err = database.Open("ffldb", filepath.Join(f.path, "blocks"), wire.MainNet)
	require.NoError(f.t, err)
	f.chain = nil
	f.openWorker()
	f.chain, err = blockchain.New(&blockchain.Config{
		DB: f.db, ChainParams: &f.params, TimeSource: blockchain.NewMedianTime(),
		SigCache: txscript.NewSigCache(100), UtxoCacheMaxSize: 1 << 20,
		IndexManager: NewManager(f.db, []Indexer{NewTxIndex(f.db), f.worker}),
	})
	require.NoError(f.t, err)
}

func (f *proofWorkerFixture) add(prev *btcutil.Block, spends []*blockchain.SpendableOut) (*btcutil.Block, []*blockchain.SpendableOut) {
	block, outs, err := blockchain.AddBlock(f.chain, prev, spends)
	require.NoError(f.t, err)
	return block, outs
}

// Compare every stored proof, root, target, leaf preimage and TTL against the
// synchronous algorithm running independently over exactly the same blocks.
func (f *proofWorkerFixture) compare() {
	oracle, err := NewFlatUtreexoProofIndex(false, &f.params, 1<<20, f.t.TempDir(), f.db.Flush)
	require.NoError(f.t, err)
	require.NoError(f.t, oracle.Init(f.chain, f.params.GenesisHash, 0))
	defer func() {
		require.NoError(f.t, oracle.utreexoState.state.CloseWithoutFlush())
		b := &BackgroundProofIndex{FlatUtreexoProofIndex: oracle}
		for _, state := range b.flatStates() {
			require.NoError(f.t, state.dataFile.Close())
			require.NoError(f.t, state.offsetFile.Close())
		}
	}()
	f.worker.access.Lock()
	defer f.worker.access.Unlock()
	for h := int32(1); h <= f.chain.BestSnapshot().Height; h++ {
		block, err := f.chain.BlockByHeight(h)
		require.NoError(f.t, err)
		stxos, err := f.chain.FetchSpendJournalForBlock(block)
		require.NoError(f.t, err)
		require.NoError(f.t, oracle.ConnectBlock(nil, block, stxos))
	}
	ref := &BackgroundProofIndex{FlatUtreexoProofIndex: oracle}
	for i, state := range ref.flatStates() {
		for h := int32(1); h <= f.chain.BestSnapshot().Height; h++ {
			want, err := state.FetchData(h)
			require.NoError(f.t, err)
			got, err := f.worker.flatStates()[i].FetchData(h)
			require.NoError(f.t, err)
			require.Equal(f.t, want, got, "state %d, height %d", i, h)
		}
	}
	require.Equal(f.t, oracle.utreexoState.state.GetRoots(), f.worker.utreexoState.state.GetRoots())
	require.Equal(f.t, oracle.utreexoRootsState.GetRoots(), f.worker.utreexoRootsState.GetRoots())
	require.InDelta(f.t, oracle.pStats.BlockProofOverheadSum, f.worker.pStats.BlockProofOverheadSum, 1e-9)
	gotStats := f.worker.pStats
	gotStats.BlockProofOverheadSum = oracle.pStats.BlockProofOverheadSum
	require.Equal(f.t, oracle.pStats, gotStats)
}

func TestBackgroundProofValidationAndReorg(t *testing.T) {
	f := newProofWorkerFixture(t)
	f.start()
	require.Eventually(t, func() bool { return !f.worker.Snapshot().Initializing }, 10*time.Second, time.Millisecond)
	genesis := btcutil.NewBlock(f.params.GenesisBlock)
	b1, outs1 := f.add(genesis, nil)
	b2, outs := f.add(b1, outs1)
	tip := b2
	for i := 0; i < 6; i++ {
		tip, outs = f.add(tip, outs)
	}
	require.Equal(t, int32(-1), f.worker.Snapshot().Height)
	_, err := f.worker.FetchUtreexoProofForHash(tip.Hash())
	require.ErrorIs(t, err, ErrProofIndexNotReady)
	f.waitSynced()
	f.compare()

	// A stalled proof operation must not hold the chain/database locks.
	f.worker.access.Lock()
	done := make(chan error, 1)
	go func() {
		_, _, err := blockchain.AddBlock(f.chain, tip, outs)
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		f.worker.access.Unlock()
		t.Fatal("validation blocked on proof worker")
	}
	_, err = f.worker.FetchUtreexoProof(1)
	require.ErrorIs(t, err, ErrProofIndexNotReady)
	f.worker.access.Unlock()

	// Reorganize while proofs are paused, then resume across the old tip.
	alt, altOuts := f.add(b1, outs1)
	for i := 0; i < 9; i++ {
		alt, altOuts = f.add(alt, altOuts)
	}
	require.Equal(t, *alt.Hash(), f.chain.BestSnapshot().Hash)
	_, err = f.worker.FetchUtreexoProofForHash(tip.Hash())
	require.ErrorIs(t, err, ErrProofIndexNotReady)
	f.waitSynced()
	f.compare()
	_, err = f.worker.FetchUtreexoProofForHash(b2.Hash())
	require.ErrorIs(t, err, ErrProofIndexNotReady)

	// A clean restart resumes the proof checkpoint, without replaying IBD.
	f.closeWorker()
	f.openWorker()
	f.waitSynced()
	f.compare()
}

func TestBackgroundProofCrashRecovery(t *testing.T) {
	for _, stage := range []string{"connect", "connect-after-wal", "partial-connect", "undo-before-wal", "undo-after-wal"} {
		t.Run(stage, func(t *testing.T) {
			f := newProofWorkerFixture(t)
			tip := btcutil.NewBlock(f.params.GenesisBlock)
			var outs []*blockchain.SpendableOut
			for i := 0; i < 5; i++ {
				tip, outs = f.add(tip, outs)
			}
			f.waitSynced()
			f.closeWorker()
			f.openWorker()
			require.NoError(t, f.worker.recover())
			b := f.worker
			if stage == "connect" || stage == "connect-after-wal" || stage == "partial-connect" {
				next, _ := f.add(tip, outs)
				plan := &proofReplayIntent{From: b.tip, To: proofTip{Hash: *next.Hash(), Height: next.Height()}}
				require.NoError(t, b.prepare(plan))
				stxos, err := f.chain.FetchSpendJournalForBlock(next)
				require.NoError(t, err)
				require.NoError(t, b.ConnectBlock(nil, next, stxos))
				if stage == "connect-after-wal" {
					b.tip = plan.To
					require.NoError(t, b.checkpoint(false))
				}
				if stage == "partial-connect" {
					// Simulate a process death before the final flat records
					// were published, with earlier in-place TTL changes saved.
					require.NoError(t, b.leafDataState.DisconnectBlock(next.Height()))
					_, err = b.leafDataState.dataFile.WriteAt([]byte{0xaa, 0xff}, b.leafDataState.currentOffset)
					require.NoError(t, err)
				}
			} else {
				plan := &proofReplayIntent{From: b.tip, To: proofTip{
					Hash: tip.MsgBlock().Header.PrevBlock, Height: tip.Height() - 1}, Disconnect: true}
				require.NoError(t, b.prepare(plan))
				if stage == "undo-before-wal" {
					adds, proof, dels, ud, err := b.getUndoData(tip)
					require.NoError(t, err)
					indexes, err := b.getCreateIndexes(tip.Hash(), ud.LeafDatas, dels)
					require.NoError(t, err)
					require.NoError(t, b.resetTTLs(ud, indexes))
					roots, err := b.fetchRoots(tip.Height() - 1)
					require.NoError(t, err)
					require.NoError(t, b.utreexoState.state.Undo(adds, *proof, dels, roots.Roots))
				} else {
					require.NoError(t, b.DisconnectBlock(nil, tip, nil))
				}
			}
			// Save flat writes, drop uncommitted accumulator changes and
			// reopen all handles without a graceful worker checkpoint.
			require.NoError(t, b.syncFlatFiles())
			f.closeWorker()
			f.reopenDatabase()
			f.waitSynced()
			f.compare()
		})
	}
}

type failingProofFlushDB struct {
	database.DB
	fail atomic.Bool
}

func (db *failingProofFlushDB) Flush() error {
	if db.fail.Load() {
		return errors.New("injected proof checkpoint flush failure")
	}
	return db.DB.Flush()
}

func TestBackgroundProofFailureKeepsValidationRunning(t *testing.T) {
	f := newProofWorkerFixture(t)
	db := &failingProofFlushDB{DB: f.db}
	f.worker.db = db
	f.start()
	require.Eventually(t, func() bool { return !f.worker.Snapshot().Initializing }, 10*time.Second, time.Millisecond)
	tip, outs := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
	db.fail.Store(true)
	f.worker.SetPaused(false)
	require.Eventually(t, func() bool { return f.worker.Snapshot().Stopped }, 10*time.Second, time.Millisecond)
	require.Contains(t, f.worker.Snapshot().LastError, "injected")
	_, _ = f.add(tip, outs)
	require.Equal(t, int32(2), f.chain.BestSnapshot().Height)
	_, err := f.worker.FetchUtreexoProof(1)
	require.ErrorIs(t, err, ErrProofIndexNotReady)
	// Failure cleanup already closed the proof files; validation's DB remains open.
	require.Error(t, f.worker.Stop())
	f.running = false
	f.reopenDatabase()
	f.waitSynced()
	f.compare()
}

func TestBackgroundProofLaggedRestartAndConcurrentReorg(t *testing.T) {
	f := newProofWorkerFixture(t)
	tip := btcutil.NewBlock(f.params.GenesisBlock)
	b1, outs1 := f.add(tip, nil)
	tip, outs := b1, outs1
	for i := 0; i < 24; i++ {
		tip, outs = f.add(tip, outs)
	}
	// Stop while paused at -1. The validating tip must not label the forest.
	f.start()
	f.closeWorker()
	f.reopenDatabase()
	require.Equal(t, int32(-1), f.worker.initialTip.Height)
	f.worker.delay = time.Millisecond
	f.start()
	f.worker.SetPaused(false)
	alt, altOuts := f.add(b1, outs1)
	for i := 0; i < 26; i++ {
		alt, altOuts = f.add(alt, altOuts)
	}
	f.waitSynced()
	f.compare()
}

func TestBackgroundProofSynchronousResume(t *testing.T) {
	f := newProofWorkerFixture(t)
	tip, outs := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
	f.waitSynced()
	for i := 0; i < 6; i++ {
		tip, outs = f.add(tip, outs)
	}
	f.closeWorker()
	f.openWorker()
	// The worker's clean checkpoint must be usable by synchronous startup,
	// including the catch-up interval after a paused shutdown.
	idx := f.worker.FlatUtreexoProofIndex
	idx.background = nil
	require.NoError(t, NewManager(f.db, []Indexer{idx}).Init(f.chain, nil))
	f.compare()
}

// Unlike closing handles without a forest flush, process exit also discards
// FFldb's cache and bypasses all file cleanup. The replay intent must preserve
// its source branch and repairs using only the actual durable checkpoints.
func TestBackgroundProofAbruptProcessExit(t *testing.T) {
	const crashCode = 23
	if stage := os.Getenv("UTREEXOD_PROOF_CRASH_STAGE"); stage != "" {
		f := proofWorkerFixtureAt(t, os.Getenv("UTREEXOD_PROOF_CRASH_PATH"), true)
		tip := btcutil.NewBlock(f.params.GenesisBlock)
		var outs []*blockchain.SpendableOut
		var fork *btcutil.Block
		var forkOuts []*blockchain.SpendableOut
		for i := 0; i < 5; i++ {
			tip, outs = f.add(tip, outs)
			if i == 0 {
				fork, forkOuts = tip, outs
			}
		}
		require.NoError(t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
		if stage == "first-connect" {
			require.NoError(t, f.worker.recover())
			b := f.worker
			require.NoError(t, b.prepare(&proofReplayIntent{From: b.tip, To: proofTip{Hash: *tip.Hash(), Height: tip.Height()}}))
			for h := int32(0); h < 3; h++ {
				block, err := f.chain.BlockByHeight(h)
				require.NoError(t, err)
				stxos, err := f.chain.FetchSpendJournalForBlock(block)
				require.NoError(t, err)
				require.NoError(t, b.ConnectBlock(nil, block, stxos))
			}
		} else {
			f.waitSynced()
			f.closeWorker()
			f.openWorker()
			require.NoError(t, f.worker.recover())
			b := f.worker
			switch stage {
			case "partial-connect", "connect-reorg":
				next, _ := f.add(tip, outs)
				require.NoError(t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
				require.NoError(t, b.prepare(&proofReplayIntent{From: b.tip, To: proofTip{Hash: *next.Hash(), Height: next.Height()}}))
				stxos, err := f.chain.FetchSpendJournalForBlock(next)
				require.NoError(t, err)
				require.NoError(t, b.ConnectBlock(nil, next, stxos))
				if stage == "partial-connect" {
					require.NoError(t, b.leafDataState.DisconnectBlock(next.Height()))
					_, err = b.leafDataState.dataFile.WriteAt([]byte{0xaa, 0xff}, b.leafDataState.currentOffset)
					require.NoError(t, err)
				} else {
					// Persist a different validated branch while both the
					// forest checkpoint and replay intent describe the old one.
					for i := 0; i < 6; i++ {
						fork, forkOuts = f.add(fork, forkOuts)
					}
					require.NoError(t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
					require.NoError(t, f.db.Flush())
				}
			case "undo-before-wal", "undo-after-wal":
				require.NoError(t, b.prepare(&proofReplayIntent{From: b.tip, To: proofTip{
					Hash: tip.MsgBlock().Header.PrevBlock, Height: tip.Height() - 1}, Disconnect: true}))
				if stage == "undo-before-wal" {
					adds, proof, dels, ud, err := b.getUndoData(tip)
					require.NoError(t, err)
					indexes, err := b.getCreateIndexes(tip.Hash(), ud.LeafDatas, dels)
					require.NoError(t, err)
					require.NoError(t, b.resetTTLs(ud, indexes))
					roots, err := b.fetchRoots(tip.Height() - 1)
					require.NoError(t, err)
					require.NoError(t, b.utreexoState.state.Undo(adds, *proof, dels, roots.Roots))
				} else {
					require.NoError(t, b.DisconnectBlock(nil, tip, nil))
				}
			default:
				t.Fatalf("unknown crash stage %q", stage)
			}
		}
		os.Exit(crashCode)
	}
	for _, stage := range []string{"first-connect", "partial-connect", "connect-reorg", "undo-before-wal", "undo-after-wal"} {
		t.Run(stage, func(t *testing.T) {
			path := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestBackgroundProofAbruptProcessExit$")
			cmd.Env = append(os.Environ(), "UTREEXOD_PROOF_CRASH_STAGE="+stage, "UTREEXOD_PROOF_CRASH_PATH="+path)
			output, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "%s", output)
			require.Equal(t, crashCode, exit.ExitCode(), "%s", output)
			f := proofWorkerFixtureAt(t, path, false)
			f.waitSynced()
			f.compare()
		})
	}
}
