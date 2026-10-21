package indexers

import (
	"encoding/json"
	"fmt"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/utreexo/utreexo"
	"github.com/utreexo/utreexod/bip182"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/database"
)

// recover runs before any reader is admitted to the index. The WAL hash is
// the authoritative accumulator checkpoint; the intent bounds every possible
// flat-file mutation beyond it, including incomplete connect/disconnect work.
func (b *BackgroundProofIndex) recover() error {
	var plan *proofReplayIntent
	err := b.db.View(func(tx database.Tx) error {
		serialized := tx.Metadata().Bucket(b.Key()).Get(proofReplayKey)
		if len(serialized) == 0 {
			return nil
		}
		if err := json.Unmarshal(serialized, &plan); err != nil {
			return err
		}
		if plan == nil {
			return fmt.Errorf("empty proof replay intent")
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read proof replay intent: %w", err)
	}
	if plan == nil {
		// This also permits enabling background indexing on existing flat
		// data and returning to synchronous indexing after a clean stop.
		if err := b.FlatUtreexoProofIndex.Init(b.chain, &b.initialTip.Hash, b.initialTip.Height); err != nil {
			return err
		}
		b.tip = b.initialTip
		if err := b.checkpointLocked(true); err != nil {
			return err
		}
		b.publish(false)
		return nil
	}
	if err := b.validateIntent(plan); err != nil {
		return err
	}
	state, savedHash, err := openUtreexoState(b.config)
	if err != nil {
		return err
	}
	b.utreexoState = state
	b.tip = proofTip{Height: -1}
	if savedHash != nil {
		height, err := b.chain.KnownBlockHeight(savedHash)
		if err != nil {
			return err
		}
		b.tip = proofTip{Hash: *savedHash, Height: height}
	}
	if plan.Disconnect {
		if b.tip != plan.From && b.tip != plan.To {
			return fmt.Errorf("proof checkpoint does not match either side of pending disconnect")
		}
		block, err := b.chain.BlockByHeightOnBranch(&plan.From.Hash, plan.From.Height)
		if err != nil {
			return err
		}
		// If the undo never committed, restore the TTL deaths it may have
		// cleared. Otherwise finish clearing them at the parent checkpoint.
		if err := b.repairTTLs(block, b.tip == plan.From); err != nil {
			return err
		}
		b.pStats = plan.FromStats
		if b.tip == plan.To {
			b.pStats = *plan.ToStats
		}
	} else {
		if b.tip.Height < plan.From.Height || b.tip.Height > plan.To.Height {
			return fmt.Errorf("proof checkpoint height %d outside replay interval [%d,%d]",
				b.tip.Height, plan.From.Height, plan.To.Height)
		}
		if b.tip.Height >= 0 {
			ancestor, err := b.chain.BlockHashByHeightOnBranch(&plan.To.Hash, b.tip.Height)
			if err != nil || *ancestor != b.tip.Hash {
				return fmt.Errorf("proof checkpoint is not on the pending replay branch")
			}
		}
		// The raw research-statistics file is updated in place. Restore
		// its checkpoint snapshot, then add only WAL-committed blocks,
		// whose proof records were synced before that WAL commit.
		b.pStats = plan.FromStats
		for height := max(plan.From.Height+1, 1); height <= b.tip.Height; height++ {
			ud, err := b.fetchUtreexoProof(height)
			if err != nil {
				return err
			}
			b.pStats.UpdateTotalDelCount(uint64(len(ud.LeafDatas)))
			b.pStats.UpdateUDStats(false, ud)
			b.pStats.BlockHeight = uint64(height)
		}
		// In-place TTL writes can outlive unflushed accumulator changes.
		// Reset every possible deletion after the checkpoint, not just
		// blocks whose last flat record happened to reach disk.
		for height := b.tip.Height + 1; height <= plan.To.Height; height++ {
			block, err := b.chain.BlockByHeightOnBranch(&plan.To.Hash, height)
			if err != nil {
				return err
			}
			if err := b.repairTTLs(block, false); err != nil {
				return err
			}
		}
	}
	if err := b.resetFlatState(); err != nil {
		return err
	}
	// Make repairs durable before forgetting their intent. Repeating them
	// after another interruption is idempotent.
	if err := b.syncFlatFiles(); err != nil {
		return err
	}
	if err := b.storeTip(true); err != nil {
		return err
	}
	if err := b.db.Flush(); err != nil {
		return err
	}
	b.checkpointTip = b.tip
	b.checkpointStats = b.pStats
	b.publish(false)
	return nil
}

func (b *BackgroundProofIndex) validateIntent(plan *proofReplayIntent) error {
	if plan.From.Height < -1 || plan.To.Height < 0 || (plan.From.Height == -1 && plan.From.Hash != (chainhash.Hash{})) {
		return fmt.Errorf("invalid proof replay heights")
	}
	for _, tip := range []proofTip{plan.From, plan.To} {
		if tip.Height == -1 {
			continue
		}
		height, err := b.chain.KnownBlockHeight(&tip.Hash)
		if err != nil || height != tip.Height {
			return fmt.Errorf("proof replay hash %v does not match height %d", tip.Hash, tip.Height)
		}
	}
	if plan.Disconnect {
		if plan.ToStats == nil {
			return fmt.Errorf("missing parent statistics for proof disconnect")
		}
		if plan.From.Height != plan.To.Height+1 {
			return fmt.Errorf("proof disconnect must cover exactly one block")
		}
		hash, err := b.chain.BlockHashByHeightOnBranch(&plan.From.Hash, plan.To.Height)
		if err != nil || *hash != plan.To.Hash {
			return fmt.Errorf("proof disconnect does not point to its parent")
		}
		return nil
	}
	if plan.From.Height > plan.To.Height {
		return fmt.Errorf("reversed proof replay interval")
	}
	if plan.From.Height >= 0 {
		hash, err := b.chain.BlockHashByHeightOnBranch(&plan.To.Hash, plan.From.Height)
		if err != nil || *hash != plan.From.Hash {
			return fmt.Errorf("proof replay start is not an ancestor of its destination")
		}
	}
	return nil
}

func (b *BackgroundProofIndex) repairTTLs(block *btcutil.Block, restore bool) error {
	if block.Height() == 0 {
		return nil
	}
	stxos, err := b.chain.FetchSpendJournalForBlock(block)
	if err != nil {
		return err
	}
	_, _, inskip, _ := blockchain.DedupeBlock(block)
	lds, err := blockchain.BlockToDelLeaves(stxos, b.chain, block, inskip)
	if err != nil {
		return err
	}
	retained := lds[:0]
	for _, ld := range lds {
		if ld.Height <= b.tip.Height {
			retained = append(retained, ld)
		}
	}
	delHashes := make([]utreexo.Hash, len(retained))
	hasher := bip182.NewLeafHasher()
	for i := range retained {
		delHashes[i] = hasher.HashLeaf(&retained[i])
	}
	indexes, err := b.getCreateIndexes(block.Hash(), retained, delHashes)
	if err != nil {
		return err
	}
	for i, ld := range retained {
		var data [ttlSize]byte
		if restore {
			// Lost undo retains every leaf, so i is its deletion index.
			byteOrder.PutUint32(data[:4], uint32(block.Height()))
			byteOrder.PutUint32(data[4:], uint32(i))
		}
		if err := b.ttlState.OverWrite(ld.Height, indexes[i]*ttlSize, data[:]); err != nil {
			return err
		}
	}
	return nil
}

func (b *BackgroundProofIndex) resetFlatState() error {
	for _, state := range b.flatStates() {
		if state == &b.proofStatsState {
			continue // statistics are derived and older pipelines omitted empty blocks
		}
		if b.tip.Height > state.BestHeight() {
			return fmt.Errorf("flat proof data stops at %d before WAL checkpoint %d", state.BestHeight(), b.tip.Height)
		}
	}
	if err := b.consistentFlatFileState(b.tip.Height); err != nil {
		return err
	}
	if err := b.pStats.WritePStats(&b.proofStatsState); err != nil {
		return err
	}
	if err := b.initUtreexoRootsState(); err != nil {
		return err
	}
	b.blockTTLState = nil
	return b.updateBlockTTLState(b.tip.Height)
}
