package integration

import (
	"testing"
	"time"

	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/integration/rpctest"
)

// Exercise the real P2P callback acknowledgements and sliding-window refills,
// rather than delivering block messages directly to the sync manager.
func TestBoundedHeaderDownload(t *testing.T) {
	newNode := func() *rpctest.Harness {
		node, err := rpctest.New(&chaincfg.SimNetParams, nil,
			[]string{"--noutreexo", "--nobdkwallet", "--prune=0"}, "")
		require.NoError(t, err)
		require.NoError(t, node.SetUp(false, 0))
		t.Cleanup(func() { require.NoError(t, node.TearDown()) })
		return node
	}
	source := newNode()
	// Fresh timestamps make the source current, so it serves getheaders.
	const blocks = 100
	hashes, err := source.Client.Generate(blocks)
	require.NoError(t, err)
	target := newNode()
	require.NoError(t, rpctest.ConnectNode(target, source))
	require.Eventually(t, func() bool {
		hash, height, err := target.Client.GetBestBlock()
		return err == nil && height == blocks && *hash == *hashes[len(hashes)-1]
	}, time.Minute, 50*time.Millisecond)
}
