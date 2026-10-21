package indexers

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/database"
)

// ErrProofIndexNotReady indicates that a proof cannot currently be served.
// Conventional validation and block serving remain available.
var ErrProofIndexNotReady = errors.New("proof index is not ready for this request")

var proofReplayKey = []byte("background-replay-v1")

type proofTip struct {
	Hash   chainhash.Hash
	Height int32
}

// proofReplayIntent is written and synced before changing any proof files.
// Connections cover a branch interval; disconnections cover exactly one block.
// Blocks and spend journals in the archival database are the durable backlog.
type proofReplayIntent struct {
	From       proofTip
	To         proofTip
	Disconnect bool
	FromStats  ProofStats
	ToStats    *ProofStats `json:",omitempty"` // parent statistics for a disconnect
}

type proofProgress struct {
	Tip          proofTip
	Initializing bool
	Processing   bool
	Stopped      bool
	Err          error
}

// ProofIndexStatus describes progress independently of the validated chain.
type ProofIndexStatus struct {
	Height          int32  `json:"height"`
	BestBlockHash   string `json:"bestblockhash"`
	ValidatedHeight int32  `json:"validatedheight"`
	Synced          bool   `json:"synced"`
	Paused          bool   `json:"paused"`
	Initializing    bool   `json:"initializing"`
	Processing      bool   `json:"processing"`
	Stopped         bool   `json:"stopped"`
	LastError       string `json:"error,omitempty"`
}

// BackgroundProofIndex owns one archival flat proof index. It processes one
// block at a time without a chain lock or a database transaction held during
// proof generation. Only small replay/checkpoint metadata updates use writable
// transactions. No block data is queued in memory by the validation path.
//
// A durable intent lets recovery repair in-place TTL changes and partial flat
// records before resuming from the forest WAL checkpoint, even after a reorg.
type BackgroundProofIndex struct {
	*FlatUtreexoProofIndex
	db    database.DB
	delay time.Duration

	access   sync.RWMutex // entire proof operation, never taken by validation
	progress atomic.Pointer[proofProgress]
	paused   atomic.Bool
	start    sync.Once
	stop     sync.Once
	quit     chan struct{}
	done     chan struct{}
	wake     chan struct{}

	// Only the worker accesses the following after Start.
	tip             proofTip
	checkpointTip   proofTip
	checkpointStats ProofStats
	dirty           bool

	initialTip proofTip // assigned by Manager.Init before Start
}

func NewBackgroundProofIndex(db database.DB, index *FlatUtreexoProofIndex,
	delay time.Duration, paused bool) (*BackgroundProofIndex, error) {
	if index.config.Pruned {
		return nil, fmt.Errorf("background proof indexing requires archived blocks and spend journals")
	}
	if delay < 0 {
		return nil, fmt.Errorf("proof index delay must not be negative")
	}
	b := &BackgroundProofIndex{
		FlatUtreexoProofIndex: index, db: db, delay: delay,
		tip: proofTip{Height: -1}, checkpointTip: proofTip{Height: -1},
		quit: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1),
	}
	b.paused.Store(paused)
	b.progress.Store(&proofProgress{Tip: proofTip{Height: -1}, Initializing: true})
	index.background = b
	return b, nil
}

// Init records the persisted tip. Expensive recovery runs in Start's worker,
// allowing the validating node to start while the proof index catches up.
func (b *BackgroundProofIndex) Init(chain *blockchain.BlockChain, hash *chainhash.Hash, height int32) error {
	b.chain = chain
	b.initialTip = proofTip{Hash: *hash, Height: height}
	return nil
}

func (b *BackgroundProofIndex) Start() {
	b.start.Do(func() { go b.run() })
}

// Stop finishes the current block and checkpoints its actual proof tip. It
// never waits for the index to catch up to the validated chain.
func (b *BackgroundProofIndex) Stop() error {
	b.Start()
	b.stop.Do(func() { close(b.quit) })
	<-b.done
	return b.progress.Load().Err
}

func (b *BackgroundProofIndex) SetPaused(paused bool) {
	b.paused.Store(paused)
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func (b *BackgroundProofIndex) Snapshot() ProofIndexStatus {
	p := b.progress.Load()
	status := ProofIndexStatus{
		Height: p.Tip.Height, BestBlockHash: p.Tip.Hash.String(),
		Paused: b.paused.Load(), Initializing: p.Initializing,
		Processing: p.Processing, Stopped: p.Stopped,
	}
	if p.Err != nil {
		status.LastError = p.Err.Error()
	}
	if b.chain != nil {
		best := b.chain.BestSnapshot()
		status.ValidatedHeight = best.Height
		status.Synced = !p.Initializing && !p.Processing && !p.Stopped && p.Err == nil && p.Tip.Hash == best.Hash
	}
	return status
}

func (b *BackgroundProofIndex) publish(processing bool) {
	b.progress.Store(&proofProgress{Tip: b.tip, Processing: processing})
}

func (b *BackgroundProofIndex) wait(delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-b.quit:
	case <-b.wake:
	case <-timer.C:
	}
}

func (b *BackgroundProofIndex) run() {
	defer close(b.done)
	b.access.Lock()
	err := b.recover()
	b.access.Unlock()
	if err == nil {
		err = b.process()
	}
	b.access.Lock()
	if b.utreexoState != nil {
		// In particular, never flush a partially applied failed operation.
		err = errors.Join(err, b.utreexoState.state.CloseWithoutFlush())
	}
	for _, state := range b.flatStates() {
		err = errors.Join(err, state.dataFile.Close(), state.offsetFile.Close())
	}
	b.progress.Store(&proofProgress{Tip: b.tip, Stopped: true, Err: err})
	b.access.Unlock()
	if err != nil {
		log.Errorf("Background proof indexing stopped at height %d: %v", b.tip.Height, err)
	}
}

func (b *BackgroundProofIndex) process() error {
	reader := proofReplayReader{fetch: b.fetchReplayBlock}
	defer reader.drain()
	var plan *proofReplayIntent
	var pending *proofReplayIntent
	var prepared chan error
	// A preparation may overlap only blocks covered by the old durable
	// intent. Join before changing branches, checkpointing, or returning.
	join := func() error {
		if prepared == nil {
			return nil
		}
		err := <-prepared
		prepared = nil
		if err == nil {
			plan = pending
		}
		pending = nil
		return err
	}
	defer func() { _ = join() }()
	progress := newBlockProgressLogger("Proof indexed", log)
	for {
		select {
		case <-b.quit:
			reader.drain()
			if err := join(); err != nil {
				return err
			}
			return b.checkpoint(true)
		default:
		}
		if b.paused.Load() {
			reader.drain()
			if err := join(); err != nil {
				return err
			}
			if b.dirty || plan != nil {
				if err := b.checkpoint(true); err != nil {
					return err
				}
				plan = nil
			}
			b.wait(time.Second)
			continue
		}
		// Each step chooses a branch from an immutable chain snapshot. The
		// worker may finish a block concurrently disconnected by validation;
		// the next iteration will undo it on that original branch.
		if b.tip.Height >= 0 && !b.chain.MainChainHasBlock(&b.tip.Hash) {
			reader.drain()
			if err := join(); err != nil {
				return err
			}
			if err := b.disconnect(); err != nil {
				return err
			}
			plan = nil
			continue
		}
		best := b.chain.BestSnapshot()
		if b.tip.Height >= best.Height {
			reader.drain()
			if err := join(); err != nil {
				return err
			}
			if plan != nil || (b.dirty && time.Since(b.lastFlushTime) >= blockchain.UtxoFlushPeriodicInterval) {
				if err := b.checkpoint(true); err != nil {
					return err
				}
				plan = nil
			}
			b.wait(time.Second)
			continue
		}
		if plan == nil || b.tip.Height >= plan.To.Height || !b.chain.MainChainHasBlock(&plan.To.Hash) {
			if err := join(); err != nil {
				return err
			}
		}
		if plan == nil || b.tip.Height >= plan.To.Height || !b.chain.MainChainHasBlock(&plan.To.Hash) {
			// Persist the intended branch before touching flat files. Batching
			// this metadata sync amortizes main-DB flushes without retaining
			// any prefetched blocks or holding a transaction during proofs.
			height := min(best.Height, b.tip.Height+128)
			hash, err := b.chain.BlockHashByHeightOnBranch(&best.Hash, height)
			if err != nil {
				return err
			}
			// A reorg may have happened since the main-chain check above.
			// Never replace an intent with a branch that excludes our tip.
			if b.tip.Height >= 0 {
				ancestor, err := b.chain.BlockHashByHeightOnBranch(hash, b.tip.Height)
				if err != nil || *ancestor != b.tip.Hash {
					continue
				}
			}
			plan = &proofReplayIntent{From: b.checkpointTip, To: proofTip{Hash: *hash, Height: height}}
			if err := b.prepare(plan); err != nil {
				return err
			}
		}
		// Queue the next bounded interval while up to 64 already-durable
		// blocks remain. A busy validation writer can then overlap useful
		// proof work. Never extend the active boundary until Flush succeeds.
		if prepared == nil && plan.To.Height-b.tip.Height <= 64 && plan.To.Height < best.Height {
			height := min(best.Height, plan.To.Height+128)
			hash, err := b.chain.BlockHashByHeightOnBranch(&best.Hash, height)
			if err != nil {
				return err
			}
			ancestor, err := b.chain.BlockHashByHeightOnBranch(hash, plan.To.Height)
			if err != nil || *ancestor != plan.To.Hash {
				continue
			}
			pending = &proofReplayIntent{From: b.checkpointTip,
				To: proofTip{Hash: *hash, Height: height}, FromStats: b.checkpointStats}
			encoded, err := json.Marshal(pending)
			if err != nil {
				return err
			}
			prepared = make(chan error, 1)
			go func(done chan<- error) { done <- b.persistIntent(encoded) }(prepared)
		}
		read := reader.take(plan.To, b.tip.Height+1)
		if read.err != nil {
			return read.err
		}
		block, stxos := read.block, read.stxos
		if block.Height() > 0 && block.MsgBlock().Header.PrevBlock != b.tip.Hash {
			return fmt.Errorf("proof replay block %v does not extend tip %v", block.Hash(), b.tip.Hash)
		}
		reader.start(plan.To, block.Height()+1)
		b.access.Lock()
		b.publish(true)
		err := b.FlatUtreexoProofIndex.ConnectBlock(nil, block, stxos)
		if err == nil {
			b.tip = proofTip{Hash: *block.Hash(), Height: block.Height()}
			b.dirty = true
			b.publish(false)
		}
		b.access.Unlock()
		if err != nil {
			return err
		}
		progress.LogBlockHeight(block)
		if b.utreexoState.state.FlushNeeded() || time.Since(b.lastFlushTime) >= blockchain.UtxoFlushPeriodicInterval {
			reader.join()
			if err := join(); err != nil {
				return err
			}
			if err := b.checkpoint(false); err != nil {
				return err
			}
		}
		if b.delay > 0 {
			b.wait(b.delay)
		}
	}
}

func (b *BackgroundProofIndex) flatStates() []*FlatFileState {
	return []*FlatFileState{&b.undoState, &b.proofStatsState, &b.rootsState,
		&b.targetState, &b.proofState, &b.leafDataState, &b.ttlState}
}

func (b *BackgroundProofIndex) syncFlatFiles() error {
	for _, state := range b.flatStates() {
		if err := state.Sync(); err != nil {
			return err
		}
	}
	return nil
}

func (b *BackgroundProofIndex) prepare(plan *proofReplayIntent) error {
	plan.FromStats = b.checkpointStats
	if plan.Disconnect {
		ud, err := b.fetchUtreexoProof(plan.From.Height)
		if err != nil {
			return err
		}
		parentStats := plan.FromStats
		parentStats.undoBlock(ud, uint64(plan.To.Height))
		plan.ToStats = &parentStats
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	return b.persistIntent(encoded)
}

// persistIntent uses only immutable input and the concurrency-safe database.
func (b *BackgroundProofIndex) persistIntent(encoded []byte) error {
	err := b.db.Update(func(tx database.Tx) error {
		return tx.Metadata().Bucket(b.Key()).Put(proofReplayKey, encoded)
	})
	if err != nil {
		return err
	}
	// This also makes the source blocks and journals durable before the
	// proof worker can mutate any state that depends on them.
	return b.db.Flush()
}

func (b *BackgroundProofIndex) storeTip(clearIntent bool) error {
	return b.db.Update(func(tx database.Tx) error {
		if err := dbPutIndexerTip(tx, b.Key(), &b.tip.Hash, b.tip.Height); err != nil {
			return err
		}
		if clearIntent {
			return tx.Metadata().Bucket(b.Key()).Delete(proofReplayKey)
		}
		return nil
	})
}

func (b *BackgroundProofIndex) checkpoint(clearIntent bool) error {
	b.access.Lock()
	defer b.access.Unlock()
	return b.checkpointLocked(clearIntent)
}

func (b *BackgroundProofIndex) checkpointLocked(clearIntent bool) error {
	if err := b.syncFlatFiles(); err != nil {
		return err
	}
	if err := b.storeTip(false); err != nil {
		return err
	}
	if err := b.db.Flush(); err != nil {
		return err
	}
	if err := b.flushUtreexoState(&b.tip.Hash); err != nil {
		return err
	}
	b.checkpointTip = b.tip
	b.checkpointStats = b.pStats
	b.dirty = false
	b.lastFlushTime = time.Now()
	if clearIntent {
		if err := b.storeTip(true); err != nil {
			return err
		}
		return b.db.Flush()
	}
	return nil
}

func (b *BackgroundProofIndex) disconnect() error {
	// A one-block disconnect starts from a fully durable checkpoint so
	// recovery can distinguish a lost undo from a committed undo.
	if err := b.checkpoint(true); err != nil {
		return err
	}
	block, err := b.chain.BlockByHeightOnBranch(&b.tip.Hash, b.tip.Height)
	if err != nil {
		return err
	}
	plan := &proofReplayIntent{From: b.tip, To: proofTip{
		Hash: block.MsgBlock().Header.PrevBlock, Height: b.tip.Height - 1}, Disconnect: true}
	if err := b.prepare(plan); err != nil {
		return err
	}
	b.access.Lock()
	b.publish(true)
	err = b.FlatUtreexoProofIndex.DisconnectBlock(nil, block, nil)
	if err == nil {
		b.tip = plan.To
		b.dirty = true
		b.pStats = *plan.ToStats
		// Proof statistics and TTL commitments also follow the new tip.
		err = b.resetFlatState()
		if err == nil {
			b.publish(false)
		}
	}
	b.access.Unlock()
	if err != nil {
		return err
	}
	return b.checkpoint(true)
}

// readLock never waits on proof generation. This matters for callbacks made
// from the validation/relay path as well as RPC latency. A lagging index can
// serve historical data on its branch; current-state proofs require equality
// with the validated tip. Returned data must be detached before unlocking.
func (b *BackgroundProofIndex) readLock(height int32, current bool) (func(), error) {
	if !b.access.TryRLock() {
		return nil, ErrProofIndexNotReady
	}
	p := b.progress.Load()
	ready := !p.Initializing && !p.Processing && !p.Stopped && p.Err == nil && height <= p.Tip.Height &&
		b.chain.MainChainHasBlock(&p.Tip.Hash)
	if current {
		ready = ready && b.chain.BestSnapshot().Hash == p.Tip.Hash
	}
	if !ready {
		b.access.RUnlock()
		return nil, ErrProofIndexNotReady
	}
	return b.access.RUnlock, nil
}

func (idx *FlatUtreexoProofIndex) proofReadLock(height int32, current bool) (func(), error) {
	if idx.background != nil {
		return idx.background.readLock(height, current)
	}
	return func() {}, nil
}

// proofBlockReadLock checks identity as well as height. A height resolved by
// the caller before a reorg must never select a proof from a different fork.
func (idx *FlatUtreexoProofIndex) proofBlockReadLock(hash *chainhash.Hash, current bool) (int32, func(), error) {
	height, err := idx.chain.KnownBlockHeight(hash)
	if err != nil {
		return 0, nil, err
	}
	unlock, err := idx.proofReadLock(height, current)
	if err != nil {
		return 0, nil, err
	}
	if idx.background != nil {
		tip := idx.background.progress.Load().Tip
		ancestor, err := idx.chain.BlockHashByHeightOnBranch(&tip.Hash, height)
		if err != nil || *ancestor != *hash {
			unlock()
			return 0, nil, ErrProofIndexNotReady
		}
	} else if !idx.chain.MainChainHasBlock(hash) {
		unlock()
		return 0, nil, ErrProofIndexNotReady
	}
	return height, unlock, nil
}
