package indexers

import (
	"errors"
	"path/filepath"
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

type addrWorkerFixture struct {
	t      *testing.T
	path   string
	params chaincfg.Params
	db     database.DB
	chain  *blockchain.BlockChain
	worker *BackgroundAddrIndex
	addr   btcutil.Address
	serial int64
}

func newAddrWorkerFixture(t *testing.T) *addrWorkerFixture {
	f := &addrWorkerFixture{t: t, path: filepath.Join(t.TempDir(), "blocks"), params: chaincfg.RegressionNetParams}
	f.params.CoinbaseMaturity = 1
	var err error
	f.addr, err = btcutil.NewAddressScriptHash([]byte{txscript.OP_TRUE}, &f.params)
	require.NoError(t, err)
	f.open(true)
	t.Cleanup(func() {
		// Tests may deliberately stop the worker with an error.
		_ = f.worker.Stop()
		require.NoError(t, f.db.Close())
	})
	return f
}

func (f *addrWorkerFixture) open(create bool) {
	var err error
	if create {
		f.db, err = database.Create("ffldb", f.path, wire.MainNet)
	} else {
		f.db, err = database.Open("ffldb", f.path, wire.MainNet)
	}
	require.NoError(f.t, err)
	f.worker = NewBackgroundAddrIndex(NewAddrIndex(f.db, &f.params))
	f.chain, err = blockchain.New(&blockchain.Config{
		DB: f.db, ChainParams: &f.params, TimeSource: blockchain.NewMedianTime(),
		SigCache: txscript.NewSigCache(100), UtxoCacheMaxSize: 1 << 20,
		IndexManager: NewManager(f.db, []Indexer{NewTxIndex(f.db), f.worker}),
	})
	require.NoError(f.t, err)
}

func (f *addrWorkerFixture) restart() {
	_ = f.worker.Stop()
	require.NoError(f.t, f.chain.FlushUtxoCache(blockchain.FlushRequired))
	require.NoError(f.t, f.db.Close())
	f.open(false)
}

// Coinbase outputs use a standard address. Spending transactions pay OP_TRUE,
// so their only reference to the address comes from the archived input script.
func (f *addrWorkerFixture) add(prev *btcutil.Block, spend *blockchain.SpendableOut) (*btcutil.Block, *blockchain.SpendableOut) {
	var spends []*blockchain.SpendableOut
	if spend != nil {
		spends = append(spends, spend)
	}
	block, _ := blockchain.NewBlock(f.chain, prev, spends)
	msg := block.MsgBlock()
	script, err := txscript.PayToAddrScript(f.addr)
	require.NoError(f.t, err)
	msg.Transactions[0].TxOut[0].PkScript = script
	f.serial++
	marker, err := txscript.NewScriptBuilder().AddOp(txscript.OP_RETURN).AddInt64(f.serial).Script()
	require.NoError(f.t, err)
	msg.Transactions[0].AddTxOut(wire.NewTxOut(0, marker))
	if spend != nil {
		msg.Transactions[1].TxIn[0].SignatureScript = []byte{1, txscript.OP_TRUE}
	}
	txs := make([]*btcutil.Tx, len(msg.Transactions))
	for i, tx := range msg.Transactions {
		txs[i] = btcutil.NewTx(tx)
	}
	merkle := blockchain.BuildMerkleTreeStore(txs, false)
	msg.Header.MerkleRoot = *merkle[len(merkle)-1]
	require.True(f.t, blockchain.SolveBlock(&msg.Header))
	block = btcutil.NewBlock(msg)
	block.SetHeight(prev.Height() + 1)
	_, _, err = f.chain.ProcessBlock(block, blockchain.BFNone)
	require.NoError(f.t, err)
	out := blockchain.MakeSpendableOutForTx(msg.Transactions[0], 0)
	return block, &out
}

func (f *addrWorkerFixture) status() AddrIndexStatus {
	s, err := f.worker.Snapshot()
	require.NoError(f.t, err)
	return s
}

func (f *addrWorkerFixture) catchUp() {
	for i := 0; i < 100 && !f.status().Synced; i++ {
		_, err := f.worker.step()
		require.NoError(f.t, err)
	}
	require.True(f.t, f.status().Synced)
}

func (f *addrWorkerFixture) history(tx database.Tx) []database.BlockRegion {
	regions, _, err := f.worker.TxRegionsForAddress(tx, f.addr, 0, 1000, false)
	require.NoError(f.t, err)
	return regions
}

func TestBackgroundAddrCatchUpRestartAndReorg(t *testing.T) {
	f := newAddrWorkerFixture(t)
	genesis := btcutil.NewBlock(f.params.GenesisBlock)
	b1, out1 := f.add(genesis, nil)
	b2, out2 := f.add(b1, out1)
	b3, _ := f.add(b2, out2)
	// Startup and validation complete even though address indexing has not run.
	require.Equal(t, int32(-1), f.status().Height)
	require.Equal(t, b3.Height(), f.status().ValidatedHeight)
	require.ErrorIs(t, f.db.View(f.worker.CheckReady), ErrAddrIndexNotReady)
	_, err := f.worker.step() // Genesis only.
	require.NoError(t, err)
	_, err = f.worker.step() // First block, including address entries.
	require.NoError(t, err)
	f.restart()
	require.Equal(t, b1.Height(), f.status().Height)
	f.catchUp()

	oldSnapshot, err := f.db.Begin(false)
	require.NoError(t, err)
	defer oldSnapshot.Rollback()
	want := f.history(oldSnapshot)
	require.Len(t, want, 5) // Three coinbases and two input-only spends.
	raw, err := oldSnapshot.FetchBlockRegions(want)
	require.NoError(t, err)
	for i, hash := range []*btcutil.Block{b1, b2, b2, b3, b3} {
		require.Equal(t, *hash.Hash(), *want[i].Hash)
		tx, err := btcutil.NewTxFromBytes(raw[i])
		require.NoError(t, err)
		txIndex := 0
		if i == 2 || i == 4 {
			txIndex = 1
		}
		require.Equal(t, hash.Transactions()[txIndex].Hash(), tx.Hash())
	}

	// Reorg with the worker idle. The tx index reuses block IDs while address
	// entries still refer to the old branch. No partial history may escape.
	alt2, altOut := f.add(b1, out1)
	alt3, altOut := f.add(alt2, altOut)
	alt4, _ := f.add(alt3, altOut)
	require.Equal(t, *alt4.Hash(), f.chain.BestSnapshot().Hash)
	require.False(t, f.status().Synced)
	require.ErrorIs(t, f.db.View(func(tx database.Tx) error {
		_, _, err := f.worker.TxRegionsForAddress(tx, f.addr, 0, 1000, false)
		return err
	}), ErrAddrIndexNotReady)
	// A pinned, previously complete snapshot must still resolve the old IDs.
	require.Equal(t, want, f.history(oldSnapshot))
	require.NoError(t, oldSnapshot.Rollback())

	// Restart on an orphaned address tip, then undo and replay the new branch.
	f.restart()
	require.Equal(t, b3.Hash().String(), f.status().BestBlockHash)
	f.catchUp()
	require.NoError(t, f.db.View(func(tx database.Tx) error {
		got := f.history(tx)
		require.Len(t, got, 7)
		for i, block := range []*btcutil.Block{b1, alt2, alt2, alt3, alt3, alt4, alt4} {
			require.Equal(t, *block.Hash(), *got[i].Hash)
		}
		return nil
	}))
	f.restart()
	require.True(t, f.status().Synced)
}

var errAddrCommit = errors.New("injected address commit failure")

type failedAddrCommit struct{ database.DB }

func (db failedAddrCommit) Update(fn func(database.Tx) error) error {
	return db.DB.Update(func(tx database.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return errAddrCommit
	})
}

func TestBackgroundAddrFailureIsAtomicAndDoesNotStopValidation(t *testing.T) {
	f := newAddrWorkerFixture(t)
	_, err := f.worker.step() // Commit genesis before failing a nonempty block.
	require.NoError(t, err)
	b1, out := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
	f.worker.db = failedAddrCommit{f.db}
	f.worker.Start()
	require.Eventually(t, func() bool { return f.status().Stopped }, 5*time.Second, time.Millisecond)
	require.ErrorIs(t, f.worker.Stop(), errAddrCommit)
	require.Equal(t, int32(0), f.status().Height)
	require.Contains(t, f.status().LastError, errAddrCommit.Error())
	require.ErrorIs(t, f.db.View(f.worker.CheckReady), ErrAddrIndexNotReady)
	// Validation uses its own database handle and can keep accepting blocks.
	b2, _ := f.add(b1, out)
	require.Equal(t, b2.Height(), f.status().ValidatedHeight)
	f.restart()
	f.catchUp()
	require.NoError(t, f.db.View(func(tx database.Tx) error {
		require.Len(t, f.history(tx), 3) // No duplicate from the rolled-back block.
		return nil
	}))
}

func TestBackgroundAddrWorkerLifecycle(t *testing.T) {
	f := newAddrWorkerFixture(t)
	f.worker.Start()
	tip := btcutil.NewBlock(f.params.GenesisBlock)
	var out *blockchain.SpendableOut
	for i := 0; i < 15; i++ {
		tip, out = f.add(tip, out)
	}
	require.Eventually(t, func() bool { return f.status().Synced || f.status().Stopped }, 10*time.Second, time.Millisecond)
	require.True(t, f.status().Synced)
	require.NoError(t, f.worker.Stop())
	require.NoError(t, f.worker.Stop())
	require.ErrorIs(t, f.db.View(f.worker.CheckReady), ErrAddrIndexNotReady)
	f.restart()
	require.True(t, f.status().Synced)
}

func TestBackgroundAddrReusesSynchronousIndex(t *testing.T) {
	f := newAddrWorkerFixture(t)
	b1, _ := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
	// The synchronous manager builds the same existing index. A new worker
	// must resume that tip rather than reset it or duplicate its entries.
	legacy := NewAddrIndex(f.db, &f.params)
	require.NoError(t, NewManager(f.db, []Indexer{legacy}).Init(f.chain, nil))
	f.restart()
	require.Equal(t, b1.Height(), f.status().Height)
	require.True(t, f.status().Synced)
	require.NoError(t, f.db.View(func(tx database.Tx) error {
		require.Len(t, f.history(tx), 1)
		return nil
	}))
}

type beforeAddrCommit struct {
	database.DB
	before func()
}

func (db beforeAddrCommit) Update(fn func(database.Tx) error) error {
	db.before()
	return db.DB.Update(fn)
}

func TestBackgroundAddrRechecksPreparedBranch(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "connect"
		if disconnect {
			name = "disconnect"
		}
		t.Run(name, func(t *testing.T) {
			f := newAddrWorkerFixture(t)
			b1, out1 := f.add(btcutil.NewBlock(f.params.GenesisBlock), nil)
			b2, out2 := f.add(b1, out1)
			b3, out3 := f.add(b2, out2)
			var before func()
			if disconnect {
				f.catchUp()
				alt2, altOut := f.add(b1, out1)
				alt3, altOut := f.add(alt2, altOut)
				f.add(alt3, altOut)
				// The worker prepares an undo of b3. Before its commit, the
				// original branch wins again, so the undo must be discarded.
				before = func() {
					b4, out4 := f.add(b3, out3)
					f.add(b4, out4)
				}
			} else {
				for i := 0; i < 2; i++ {
					_, err := f.worker.step() // Genesis and b1.
					require.NoError(t, err)
				}
				// The worker prepares b2 while it is canonical. Validation
				// replaces that branch before the worker acquires the writer.
				before = func() {
					alt2, altOut := f.add(b1, out1)
					alt3, altOut := f.add(alt2, altOut)
					f.add(alt3, altOut)
				}
			}
			initial := f.status()
			f.worker.db = beforeAddrCommit{DB: f.db, before: before}
			block, err := f.worker.step()
			require.NoError(t, err)
			require.Nil(t, block)
			require.Equal(t, initial.BestBlockHash, f.status().BestBlockHash)
			f.worker.db = f.db
			f.catchUp()
			require.NoError(t, f.db.View(func(tx database.Tx) error {
				got := f.history(tx)
				require.Len(t, got, int(2*f.status().Height-1))
				for _, region := range got {
					require.True(t, f.chain.MainChainHasBlock(region.Hash))
				}
				return nil
			}))
		})
	}
}
