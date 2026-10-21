package netsync

import (
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	peerpkg "github.com/utreexo/utreexod/peer"
)

const (
	headerDownloadWindow   = 32
	headerDownloadPeers    = 4
	headerDownloadsPerPeer = 8
)

// Requests and received blocks share the same bounded window. All access is
// owned by blockHandler. Compact-state IBD retains its proof/TTL scheduler.
type headerDownload struct {
	peer     *peerpkg.Peer
	height   int32
	progress time.Time
	block    *blockMsg
}

func (sm *SyncManager) fetchParallelHeaderBlocks() bool {
	if !sm.headersFirstMode || sm.chain.IsUtreexoViewActive() {
		return false
	}
	_, tip := sm.chain.BestHeader()
	fork := sm.chain.BestChainHeaderForkHeight()
	// Small catch-ups retain the ordinary announcement path. Once started,
	// finish the bounded window even when fewer than 32 blocks remain.
	if sm.headerDownloads == nil && tip-fork <= headerDownloadWindow {
		return false
	}
	if sm.headerDownloads == nil {
		// Do not adopt an unbounded legacy batch midway through delivery.
		// Let its authorized responses drain before enabling this scheduler.
		for _, state := range sm.peerStates {
			if len(state.requestedBlocks) != 0 {
				return false
			}
		}
		sm.headerDownloads = make(map[chainhash.Hash]*headerDownload)
	}
	segwit, err := sm.chain.IsDeploymentActive(chaincfg.DeploymentSegwit)
	if err != nil {
		log.Warnf("Cannot select block download peers: %v", err)
		return true
	}
	peers := make([]*peerpkg.Peer, 0, headerDownloadPeers)
	addPeer := func(p *peerpkg.Peer) {
		if len(peers) == headerDownloadPeers {
			return
		}
		for _, existing := range peers {
			if existing == p {
				return
			}
		}
		state := sm.peerStates[p]
		if state != nil && state.syncCandidate && (!segwit || p.IsWitnessEnabled()) {
			peers = append(peers, p)
		}
	}
	// Keep the active pool stable across refills rather than selecting a
	// fresh random subset from the peer map for every processed block.
	for _, request := range sm.headerDownloads {
		addPeer(request.peer)
	}
	addPeer(sm.syncPeer)
	for p := range sm.peerStates {
		if len(peers) == headerDownloadPeers {
			break
		}
		if p != sm.syncPeer {
			addPeer(p)
		}
	}
	messages := make(map[*peerpkg.Peer]*wire.MsgGetData)
	// Restrict lookahead to a window after the first missing block, including
	// buffered responses. A fast peer cannot run arbitrarily ahead of a hole.
	var firstMissing int32
	for height := fork + 1; height <= tip && len(sm.headerDownloads) < headerDownloadWindow; height++ {
		if firstMissing != 0 && height-firstMissing >= headerDownloadWindow {
			break
		}
		hash, err := sm.chain.HeaderHashByHeight(height)
		if err != nil {
			break
		}
		have, err := sm.chain.HaveBlock(hash)
		if err != nil {
			break
		}
		if have {
			continue
		}
		if firstMissing == 0 {
			firstMissing = height
		}
		if _, exists := sm.headerDownloads[*hash]; exists {
			continue
		}
		var selected *peerpkg.Peer
		load := headerDownloadsPerPeer
		for _, p := range peers {
			if max(p.LastBlock(), p.StartingHeight()) < height {
				continue
			}
			if n := len(sm.peerStates[p].requestedBlocks); n < load {
				selected, load = p, n
			}
		}
		if selected == nil {
			break
		}
		state := sm.peerStates[selected]
		state.requestedBlocks[*hash] = struct{}{}
		sm.headerDownloads[*hash] = &headerDownload{
			peer: selected, height: height, progress: time.Now(),
		}
		msg := messages[selected]
		if msg == nil {
			msg = wire.NewMsgGetDataSizeHint(headerDownloadsPerPeer)
			messages[selected] = msg
		}
		kind := wire.InvTypeBlock
		if selected.IsWitnessEnabled() {
			kind = wire.InvTypeWitnessBlock
		}
		msg.AddInvVect(wire.NewInvVect(kind, hash))
	}
	for p, msg := range messages {
		p.QueueMessage(msg, nil)
	}
	return true
}

// receiveHeaderBlock releases the network callback after buffering a response,
// allowing other peers to fill the window while its earliest block is pending.
// Only an explicitly requested hash from its assigned peer can enter the buffer.
func (sm *SyncManager) receiveHeaderBlock(msg *blockMsg) bool {
	hash := *msg.block.Hash()
	request := sm.headerDownloads[hash]
	if request == nil || request.peer != msg.peer {
		return false
	}
	if request.block != nil {
		msg.peer.Disconnect()
		return true
	}
	request.block = msg
	now := time.Now()
	for _, r := range sm.headerDownloads {
		if r.peer == msg.peer {
			r.progress = now
		}
	}
	sm.drainHeaderBlocks()
	sm.fetchHeaderBlocks(nil)
	return true
}

func (sm *SyncManager) drainHeaderBlocks() {
	for {
		var nextHash chainhash.Hash
		var next *headerDownload
		for hash, request := range sm.headerDownloads {
			if request.block == nil {
				continue
			}
			bestHash, err := sm.chain.HeaderHashByHeight(request.height)
			if err != nil || *bestHash != hash {
				// Keep outstanding stale requests authorized until they arrive;
				// discard their payload instead of validating an obsolete branch.
				delete(sm.headerDownloads, hash)
				delete(sm.peerStates[request.peer].requestedBlocks, hash)
				continue
			}
			if next == nil || request.height < next.height {
				nextHash, next = hash, request
			}
		}
		if next == nil {
			return
		}
		parent := &next.block.block.MsgBlock().Header.PrevBlock
		have, err := sm.chain.HaveBlock(parent)
		if err != nil || !have {
			return
		}
		delete(sm.headerDownloads, nextHash)
		sm.processBlockMsg(next.block)
		have, err = sm.chain.HaveBlock(&nextHash)
		if err != nil || !have {
			// Do not repeatedly request a rejected block from the same peer.
			sm.peerStates[next.peer].syncCandidate = false
			next.peer.Disconnect()
			return
		}
		sm.lastProgressTime = time.Now()
	}
}

func (sm *SyncManager) expireHeaderDownloads(now time.Time) {
	for _, request := range sm.headerDownloads {
		if request.block == nil && now.Sub(request.progress) > maxStallDuration {
			// Other downloaders making progress must not conceal a stalled
			// owner of the earliest missing block. DonePeer releases its slots.
			sm.peerStates[request.peer].syncCandidate = false
			request.peer.Disconnect()
		}
	}
}
