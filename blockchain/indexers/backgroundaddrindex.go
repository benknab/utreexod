package indexers

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/database"
)

// ErrAddrIndexNotReady means address history is not complete at the chain tip.
var ErrAddrIndexNotReady = errors.New("address index is syncing or unavailable; check getaddrindexinfo")

// AddrIndexStatus reports committed address-index progress. The transaction
// index advances atomically with validation and supplies the target tip.
type AddrIndexStatus struct {
	Height          int32  `json:"height"`
	BestBlockHash   string `json:"bestblockhash"`
	ValidatedHeight int32  `json:"validatedheight"`
	Synced          bool   `json:"synced"`
	Stopped         bool   `json:"stopped"`
	LastError       string `json:"error,omitempty"`
}

type addrWorkerState struct {
	stopped bool
	err     error
}

// BackgroundAddrIndex reuses the address index's database format. Each block's
// entries and tip commit together, so restart needs no separate checkpoint.
// Only this worker changes the address index; validation maintains the tx index.
type BackgroundAddrIndex struct {
	*AddrIndex
	chain *blockchain.BlockChain
	start sync.Once
	stop  sync.Once
	quit  chan struct{}
	done  chan struct{}
	state atomic.Pointer[addrWorkerState]
}

func NewBackgroundAddrIndex(index *AddrIndex) *BackgroundAddrIndex {
	b := &BackgroundAddrIndex{AddrIndex: index, quit: make(chan struct{}), done: make(chan struct{})}
	b.state.Store(&addrWorkerState{})
	index.background = b
	return b
}

// Init defers catch-up and orphan rollback until after the server starts.
func (b *BackgroundAddrIndex) Init(chain *blockchain.BlockChain, _ *chainhash.Hash, _ int32) error {
	b.chain = chain
	return nil
}

func (b *BackgroundAddrIndex) Start() {
	b.start.Do(func() { go b.run() })
}

// Stop finishes at most the current block, rather than waiting for catch-up.
func (b *BackgroundAddrIndex) Stop() error {
	b.stop.Do(func() { close(b.quit) })
	b.Start()
	<-b.done
	return b.state.Load().err
}

func (b *BackgroundAddrIndex) Snapshot() (AddrIndexStatus, error) {
	var status AddrIndexStatus
	err := b.db.View(func(tx database.Tx) error {
		var err error
		status, err = b.snapshot(tx)
		return err
	})
	return status, err
}

func (b *BackgroundAddrIndex) snapshot(tx database.Tx) (AddrIndexStatus, error) {
	hash, height, err := dbFetchIndexerTip(tx, addrIndexKey)
	if err != nil {
		return AddrIndexStatus{}, err
	}
	target, targetHeight, err := dbFetchIndexerTip(tx, txIndexKey)
	if err != nil {
		return AddrIndexStatus{}, err
	}
	state := b.state.Load()
	status := AddrIndexStatus{
		Height: height, BestBlockHash: hash.String(), ValidatedHeight: targetHeight,
		Stopped: state.stopped,
		Synced:  !state.stopped && state.err == nil && height >= 0 && height == targetHeight && *hash == *target,
	}
	if state.err != nil {
		status.LastError = state.err.Error()
	}
	return status, nil
}

// CheckReady uses the caller's snapshot, including the transaction index's tip
// and block-ID mappings. A separate status check cannot protect a later read
// against a reorganization that reuses those IDs.
func (idx *AddrIndex) CheckReady(tx database.Tx) error {
	if idx.background == nil {
		return nil
	}
	status, err := idx.background.snapshot(tx)
	if err != nil {
		return err
	}
	if !status.Synced {
		return ErrAddrIndexNotReady
	}
	return nil
}

func (b *BackgroundAddrIndex) run() {
	err := b.process()
	b.state.Store(&addrWorkerState{stopped: true, err: err})
	if err != nil {
		log.Errorf("Background address indexing stopped: %v", err)
	}
	close(b.done)
}

func (b *BackgroundAddrIndex) process() error {
	progress := newBlockProgressLogger("Address indexed", log)
	for {
		select {
		case <-b.quit:
			return nil
		default:
		}
		block, idle, err := b.step()
		if err != nil {
			return err
		}
		if block != nil {
			progress.LogBlockHeight(block)
		}
		if !idle {
			continue
		}
		select {
		case <-b.quit:
			return nil
		case <-time.After(time.Second):
		}
	}
}

// step prepares one block without holding a writer or chain lock. The write
// transaction rechecks branch membership against the foreground tx index.
// A reorg can invalidate preparation; in that case the next step retries.
// idle is true only when both index tips match, not after rollback or retry.
func (b *BackgroundAddrIndex) step() (block *btcutil.Block, idle bool, err error) {
	var tip, target *chainhash.Hash
	var height, targetHeight int32
	var disconnect bool
	err = b.db.View(func(tx database.Tx) error {
		var err error
		tip, height, err = dbFetchIndexerTip(tx, addrIndexKey)
		if err != nil {
			return err
		}
		target, targetHeight, err = dbFetchIndexerTip(tx, txIndexKey)
		if err != nil || height < 0 {
			return err
		}
		_, err = dbFetchBlockIDByHash(tx, tip)
		disconnect = errors.Is(err, errNoBlockIDEntry)
		if disconnect {
			return nil
		}
		return err
	})
	if err != nil {
		return nil, false, err
	}
	if !disconnect && height == targetHeight && *tip == *target {
		return nil, true, nil
	}
	anchor, nextHeight := target, height+1
	if disconnect {
		anchor, nextHeight = tip, height
	}
	block, err = b.chain.BlockByHeightOnBranch(anchor, nextHeight)
	if err != nil {
		return nil, false, err
	}
	stxos, err := b.chain.FetchSpendJournalForBlock(block)
	if err != nil {
		return nil, false, err
	}
	data := make(writeIndexData)
	b.indexBlock(data, block, stxos)
	txLocs, err := block.TxLoc()
	if err != nil {
		return nil, false, err
	}
	applied := false
	err = b.db.Update(func(tx database.Tx) error {
		current, currentHeight, err := dbFetchIndexerTip(tx, addrIndexKey)
		if err != nil {
			return err
		}
		if *current != *tip || currentHeight != height {
			return fmt.Errorf("address index changed outside its worker")
		}
		blockID, err := dbFetchBlockIDByHash(tx, block.Hash())
		missing := errors.Is(err, errNoBlockIDEntry)
		if err != nil && !missing {
			return err
		}
		if disconnect != missing {
			return nil // A reorg invalidated the prepared operation.
		}
		if disconnect {
			if err := disconnectAddrEntries(tx, data); err != nil {
				return err
			}
			return dbPutIndexerTip(tx, addrIndexKey, &block.MsgBlock().Header.PrevBlock, height-1)
		}
		if block.MsgBlock().Header.PrevBlock != *tip {
			return fmt.Errorf("address replay block does not extend its tip")
		}
		if err := connectAddrEntries(tx, blockID, txLocs, data); err != nil {
			return err
		}
		err = dbPutIndexerTip(tx, addrIndexKey, block.Hash(), nextHeight)
		applied = err == nil
		return err
	})
	if err != nil || !applied {
		return nil, false, err
	}
	return block, false, nil
}
