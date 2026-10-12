package indexers

import (
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
)

type proofReplayBlock struct {
	block *btcutil.Block
	stxos []blockchain.SpentTxOut
	err   error
}

// proofReplayReader owns at most one next-block read. Requests are bound to an
// immutable branch anchor, never a moving best-chain height. Every read joins
// before a checkpoint, reorg, pause, shutdown, or return from replay.
type proofReplayReader struct {
	fetch  func(proofTip, int32) proofReplayBlock
	anchor proofTip
	height int32
	done   chan proofReplayBlock
	ready  *proofReplayBlock
}

func (r *proofReplayReader) join() {
	if r.done != nil {
		result := <-r.done
		r.ready = &result
		r.done = nil
	}
}

func (r *proofReplayReader) drain() {
	r.join()
	r.ready = nil
}

func (r *proofReplayReader) take(anchor proofTip, height int32) proofReplayBlock {
	r.join()
	if r.ready != nil {
		result := r.ready
		r.ready = nil
		if r.anchor == anchor && r.height == height && (result.block != nil || result.err != nil) {
			return *result
		}
	}
	return r.fetch(anchor, height)
}

func (r *proofReplayReader) start(anchor proofTip, height int32) {
	// Never read past the active durable intent or queue multiple blocks.
	if r.done != nil || r.ready != nil || height > anchor.Height {
		return
	}
	r.anchor, r.height = anchor, height
	r.done = make(chan proofReplayBlock, 1)
	go func(done chan<- proofReplayBlock) {
		result := r.fetch(anchor, height)
		// Bound retained serialized payload and journal storage. Exceptionally
		// large reads fall back to the synchronous path when actually needed.
		// This is a retention budget, not a bound on transient decode memory.
		if result.block != nil && result.err == nil {
			size := int64(result.block.MsgBlock().SerializeSize()) + int64(len(result.stxos))*64
			for _, stxo := range result.stxos {
				size += int64(len(stxo.PkScript))
			}
			if size > 16<<20 {
				result = proofReplayBlock{}
			}
		}
		done <- result
	}(r.done)
}

func (b *BackgroundProofIndex) fetchReplayBlock(anchor proofTip, height int32) proofReplayBlock {
	block, err := b.chain.BlockByHeightOnBranch(&anchor.Hash, height)
	if err != nil {
		return proofReplayBlock{err: err}
	}
	stxos, err := b.chain.FetchSpendJournalForBlock(block)
	return proofReplayBlock{block: block, stxos: stxos, err: err}
}
