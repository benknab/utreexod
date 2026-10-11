package blockchain

import (
	"fmt"

	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/database"
)

// branchNodes pins ancestry to an immutable block node. Background indexers
// must not resolve historical input heights against a best chain that can
// reorganize between reads. Use the chain view's O(1) lookup where possible;
// otherwise the immutable ancestor links also work for disconnected blocks.
type blockNodeSource interface {
	NodeByHeight(int32) *blockNode
}

type branchNodes struct {
	tip  *blockNode
	best *chainView
}

func (v branchNodes) NodeByHeight(height int32) *blockNode {
	v.best.mtx.Lock()
	if v.best.contains(v.tip) && height <= v.tip.height {
		node := v.best.nodeByHeight(height)
		v.best.mtx.Unlock()
		return node
	}
	v.best.mtx.Unlock()
	return v.tip.Ancestor(height)
}

func (b *BlockChain) branchNodes(hash *chainhash.Hash) (branchNodes, error) {
	node := b.index.LookupNode(hash)
	if node == nil {
		return branchNodes{}, fmt.Errorf("unknown branch block %v", hash)
	}
	return branchNodes{tip: node, best: b.bestChain}, nil
}

// BlockHashByHeightOnBranch returns an ancestor of the supplied block, even
// if that block has since been disconnected. The result does not change when
// the best chain changes. This function is safe for concurrent access.
func (b *BlockChain) BlockHashByHeightOnBranch(tip *chainhash.Hash, height int32) (*chainhash.Hash, error) {
	branch, err := b.branchNodes(tip)
	if err != nil {
		return nil, err
	}
	node := branch.NodeByHeight(height)
	if node == nil {
		return nil, fmt.Errorf("block %v has no ancestor at height %d", tip, height)
	}
	hash := node.hash
	return &hash, nil
}

// BlockByHeightOnBranch fetches an archived block on the supplied branch. It
// holds no chain lock during database I/O and also supports disconnected blocks.
// This function is safe for concurrent access. Pruned data may be unavailable.
func (b *BlockChain) BlockByHeightOnBranch(tip *chainhash.Hash, height int32) (*btcutil.Block, error) {
	hash, err := b.BlockHashByHeightOnBranch(tip, height)
	if err != nil {
		return nil, err
	}
	var block *btcutil.Block
	err = b.db.View(func(tx database.Tx) error {
		serialized, err := tx.FetchBlock(hash)
		if err != nil {
			return err
		}
		block, err = btcutil.NewBlockFromBytes(serialized)
		return err
	})
	if err != nil {
		return nil, err
	}
	block.SetHeight(height)
	return block, nil
}
