package integration

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/chaincfg"
	"github.com/utreexo/utreexod/integration/rpctest"
	"github.com/utreexo/utreexod/wire"
)

func TestBackgroundProofRPCAndServices(t *testing.T) {
	bridge, err := rpctest.New(&chaincfg.RegressionNetParams, nil, []string{
		"--flatutreexoproofindex", "--backgroundproofindex", "--proofindexpaused",
		"--prune=0", "--nobdkwallet", "--txindex",
	}, "")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.TearDown()) })
	require.NoError(t, bridge.SetUp(false, 0))
	blocks, err := bridge.Client.Generate(10)
	require.NoError(t, err)
	status, err := bridge.Client.GetProofIndexInfo()
	require.NoError(t, err)
	require.True(t, status.Paused)
	require.Equal(t, int32(-1), status.Height)
	require.Equal(t, int32(10), status.ValidatedHeight)
	_, err = bridge.Client.GetUtreexoProof(blocks[9])
	require.Error(t, err)

	observeServices := func() wire.ServiceFlag {
		observer, err := rpctest.New(&chaincfg.RegressionNetParams, nil,
			[]string{"--noutreexo", "--nobdkwallet"}, "")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, observer.TearDown()) })
		require.NoError(t, observer.SetUp(false, 0))
		return peerServices(t, observer, bridge)
	}
	services := observeServices()
	require.NotZero(t, services&wire.SFNodeNetwork)
	require.Zero(t, services&(wire.SFNodeUtreexo|wire.SFNodeUtreexoArchive))

	waitSynced := func() {
		_, err := bridge.Client.SetProofIndexPaused(false)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			status, err := bridge.Client.GetProofIndexInfo()
			return err == nil && status.Synced
		}, 15*time.Second, 10*time.Millisecond)
		_, err = bridge.Client.SetProofIndexPaused(true)
		require.NoError(t, err)
	}
	waitSynced()
	require.Eventually(t, func() bool {
		_, err := bridge.Client.GetUtreexoProof(blocks[9])
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	services = observeServices()
	require.Equal(t, wire.ServiceFlag(wire.SFNodeUtreexo|wire.SFNodeUtreexoArchive),
		services&(wire.SFNodeUtreexo|wire.SFNodeUtreexoArchive))

	// A paused proof index must neither reject nor stall RPC invalidation.
	require.NoError(t, bridge.Client.InvalidateBlock(blocks[8]))
	newBlocks, err := bridge.Client.Generate(3)
	require.NoError(t, err)
	waitSynced()
	require.Eventually(t, func() bool {
		_, err := bridge.Client.GetUtreexoRoots(newBlocks[2])
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	_, err = bridge.Client.GetUtreexoProof(blocks[9])
	require.Error(t, err)
}
