package indexers

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/blockchain"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/wire"
)

func TestProofReplayReaderBranchAndBoundary(t *testing.T) {
	first := proofTip{Height: 10}
	second := proofTip{Height: 10}
	second.Hash[0] = 1
	staleErr, wantedErr := errors.New("old branch"), errors.New("wanted branch")
	var calls atomic.Int32
	reader := proofReplayReader{fetch: func(anchor proofTip, height int32) proofReplayBlock {
		calls.Add(1)
		require.LessOrEqual(t, height, anchor.Height)
		if anchor == first {
			return proofReplayBlock{err: staleErr}
		}
		return proofReplayBlock{err: wantedErr}
	}}
	reader.start(first, 11)
	require.Nil(t, reader.done)
	reader.start(first, 9)
	reader.start(first, 10) // Must not replace or leak an existing read.
	require.ErrorIs(t, reader.take(second, 9).err, wantedErr)
	require.Equal(t, int32(2), calls.Load())
	reader.start(second, 10)
	reader.join() // Checkpoint joins retain a usable result without refetching.
	require.Nil(t, reader.done)
	require.ErrorIs(t, reader.take(second, 10).err, wantedErr)
	require.Equal(t, int32(3), calls.Load())
	require.Nil(t, reader.done)
}

func TestProofReplayReaderRetentionBudget(t *testing.T) {
	var calls atomic.Int32
	block := btcutil.NewBlock(&wire.MsgBlock{})
	reader := proofReplayReader{fetch: func(proofTip, int32) proofReplayBlock {
		calls.Add(1)
		return proofReplayBlock{block: block,
			stxos: []blockchain.SpentTxOut{{PkScript: make([]byte, 16<<20)}}}
	}}
	anchor := proofTip{Height: 10}
	reader.start(anchor, 1)
	reader.join()
	require.Nil(t, reader.ready.block)
	require.Nil(t, reader.ready.stxos)
	result := reader.take(anchor, 1)
	require.Same(t, block, result.block)
	require.Equal(t, int32(2), calls.Load())
}

func TestProofReplayReaderDrainJoins(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	reader := proofReplayReader{fetch: func(proofTip, int32) proofReplayBlock {
		close(started)
		<-release
		finished.Store(true)
		return proofReplayBlock{err: errors.New("discarded speculative read")}
	}}
	reader.start(proofTip{Height: 10}, 1)
	<-started
	done := make(chan struct{})
	go func() { reader.drain(); close(done) }()
	select {
	case <-done:
		t.Fatal("drain returned while fetch was still blocked")
	default:
	}
	close(release)
	<-done
	require.True(t, finished.Load())
	require.Nil(t, reader.done)
	reader.drain()
}
