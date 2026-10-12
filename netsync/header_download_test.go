package netsync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/wire"
)

func parallelDownloadFixture(t *testing.T, count int) (*SyncManager, []*btcutil.Block) {
	t.Helper()
	params := chaincfg.RegressionNetParams
	params.Checkpoints = nil
	sm, cleanup := makeMockSyncManager(t, &params)
	t.Cleanup(cleanup)
	blocks := generateTestBlocks(t, &params, count)
	for _, block := range blocks {
		_, err := sm.chain.ProcessBlockHeader(&block.MsgBlock().Header, blockchain.BFNone)
		require.NoError(t, err)
	}
	for range headerDownloadPeers {
		p := newSyncCandidate(t, sm, int32(count))
		if sm.syncPeer == nil {
			sm.syncPeer = p
		}
	}
	sm.headersFirstMode = true
	sm.fetchHeaderBlocks(nil)
	return sm, blocks
}

func checkDownloadBounds(t *testing.T, sm *SyncManager) {
	t.Helper()
	require.LessOrEqual(t, len(sm.headerDownloads), headerDownloadWindow)
	requested := 0
	for _, state := range sm.peerStates {
		require.LessOrEqual(t, len(state.requestedBlocks), headerDownloadsPerPeer)
		requested += len(state.requestedBlocks)
	}
	require.Equal(t, len(sm.headerDownloads), requested)
}

func deliverDownload(t *testing.T, sm *SyncManager, block *btcutil.Block) {
	t.Helper()
	r := sm.headerDownloads[*block.Hash()]
	require.NotNil(t, r)
	sm.handleBlockMsg(&blockMsg{block: block, peer: r.peer})
	checkDownloadBounds(t, sm)
}

func TestParallelHeaderDownloadOrderingAndBounds(t *testing.T) {
	sm, blocks := parallelDownloadFixture(t, 100)
	require.Len(t, sm.headerDownloads, headerDownloadWindow)
	for _, state := range sm.peerStates {
		require.Len(t, state.requestedBlocks, headerDownloadsPerPeer)
	}
	// Deliver the window backwards, including blocks from every peer. The
	// chain must not grow an orphan pool or validate children before parents.
	for i := headerDownloadWindow - 1; i > 0; i-- {
		deliverDownload(t, sm, blocks[i])
		require.Zero(t, sm.chain.BestSnapshot().Height)
		require.False(t, sm.chain.IsKnownOrphan(blocks[i].Hash()))
	}
	deliverDownload(t, sm, blocks[0])
	require.EqualValues(t, headerDownloadWindow, sm.chain.BestSnapshot().Height)
	for _, block := range blocks[headerDownloadWindow:] {
		deliverDownload(t, sm, block)
	}
	require.EqualValues(t, len(blocks), sm.chain.BestSnapshot().Height)
	require.False(t, sm.headersFirstMode)
	require.Empty(t, sm.headerDownloads)
}

func TestParallelHeaderDownloadDisconnectAndNotFound(t *testing.T) {
	for _, notFound := range []bool{false, true} {
		sm, blocks := parallelDownloadFixture(t, 100)
		owner := sm.headerDownloads[*blocks[0].Hash()].peer
		if notFound {
			msg := wire.NewMsgNotFound()
			require.NoError(t, msg.AddInvVect(wire.NewInvVect(wire.InvTypeBlock, blocks[0].Hash())))
			sm.handleNotFoundMsg(&notFoundMsg{peer: owner, notFound: msg})
			require.False(t, sm.peerStates[owner].syncCandidate)
		} else {
			for _, request := range sm.headerDownloads {
				if request.peer == owner {
					request.progress = time.Now().Add(-2 * maxStallDuration)
				}
			}
			sm.handleStallSample()
			require.False(t, sm.peerStates[owner].syncCandidate)
		}
		sm.handleDonePeerMsg(owner)
		// A replacement supplies capacity, and the earliest hole is retried.
		newSyncCandidate(t, sm, 100)
		sm.fetchHeaderBlocks(nil)
		require.NotEqual(t, owner, sm.headerDownloads[*blocks[0].Hash()].peer)
		for _, block := range blocks {
			deliverDownload(t, sm, block)
		}
		require.EqualValues(t, len(blocks), sm.chain.BestSnapshot().Height)
	}
}

func TestParallelHeaderDownloadRejectsInvalidPayload(t *testing.T) {
	sm, blocks := parallelDownloadFixture(t, 40)
	owner := sm.headerDownloads[*blocks[0].Hash()].peer
	// Correct requested header/hash, invalid transaction data.
	bad := btcutil.NewBlock(&wire.MsgBlock{Header: blocks[0].MsgBlock().Header})
	deliverDownload(t, sm, bad)
	require.Zero(t, sm.chain.BestSnapshot().Height)
	require.False(t, sm.peerStates[owner].syncCandidate)
}

func TestParallelHeaderDownloadReorg(t *testing.T) {
	sm, old := parallelDownloadFixture(t, 50)
	for _, block := range old[:5] {
		deliverDownload(t, sm, block)
	}
	// Save a requested old-branch response, then make a longer fork win.
	stale := old[10]
	staleOwner := sm.headerDownloads[*stale.Hash()].peer
	params := *sm.chainParams
	branch := generateTestBlocks(t, &params, 51)
	previous := *params.GenesisHash
	for i, block := range branch {
		msg := block.MsgBlock()
		msg.Header.PrevBlock = previous
		msg.Header.Timestamp = msg.Header.Timestamp.Add(time.Second)
		require.True(t, solveTestBlock(&msg.Header, &params))
		branch[i] = btcutil.NewBlock(msg)
		previous = *branch[i].Hash()
		_, err := sm.chain.ProcessBlockHeader(&msg.Header, blockchain.BFNone)
		require.NoError(t, err)
	}
	sm.handleBlockMsg(&blockMsg{block: stale, peer: staleOwner})
	require.NotContains(t, sm.headerDownloads, *stale.Hash())
	have, err := sm.chain.HaveBlock(stale.Hash())
	require.NoError(t, err)
	require.False(t, have)
	// Let all remaining old responses arrive. They must release their slots
	// without becoming orphans or being treated as unsolicited messages.
	var staleHashes []chainhash.Hash
	for hash, request := range sm.headerDownloads {
		if *old[request.height-1].Hash() == hash {
			staleHashes = append(staleHashes, hash)
		}
	}
	for _, hash := range staleHashes {
		r := sm.headerDownloads[hash]
		sm.handleBlockMsg(&blockMsg{block: old[r.height-1], peer: r.peer})
	}
	for p := range sm.peerStates {
		p.UpdateLastBlockHeight(51)
	}
	sm.fetchHeaderBlocks(nil)
	for _, block := range branch {
		deliverDownload(t, sm, block)
	}
	require.Equal(t, *branch[len(branch)-1].Hash(), sm.chain.BestSnapshot().Hash)
}
