package blockchain

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/wire"
)

// A delayed index must reconstruct the same leaves after its input block is
// disconnected, including inputs created above the forks' common ancestor.
func TestProofReplayPinsBlockAncestry(t *testing.T) {
	chain, params, cleanup := utxoCacheTestChain(t.Name())
	t.Cleanup(cleanup)
	b1, spends1, err := AddBlock(chain, btcutil.NewBlock(params.GenesisBlock), nil)
	require.NoError(t, err)
	a2, spends2, err := AddBlock(chain, b1, spends1)
	require.NoError(t, err)
	a3, _, err := AddBlock(chain, a2, spends2)
	require.NoError(t, err)
	stxos, err := chain.FetchSpendJournal(a3)
	require.NoError(t, err)
	_, _, inskip, _ := DedupeBlock(a3)
	want, err := BlockToDelLeaves(stxos, chain, a3, inskip)
	require.NoError(t, err)
	var compact bytes.Buffer
	require.NoError(t, wire.SerializeUtxoData(&compact, want))

	other, outs, err := AddBlock(chain, b1, spends1)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		other, outs, err = AddBlock(chain, other, outs)
		require.NoError(t, err)
	}
	require.Equal(t, *other.Hash(), chain.BestSnapshot().Hash)
	require.False(t, chain.MainChainHasBlock(a3.Hash()))

	got, err := BlockToDelLeaves(stxos, chain, a3, inskip)
	require.NoError(t, err)
	require.Equal(t, want, got)
	lds, err := wire.DeserializeUtxoData(bytes.NewReader(compact.Bytes()))
	require.NoError(t, err)
	ud := &wire.UData{LeafDatas: lds}
	hashes, err := chain.ReconstructUData(ud, *a3.Hash())
	require.NoError(t, err)
	for i := range want {
		require.Equal(t, want[i].LeafHash(), [32]byte(hashes[i]))
	}
	archived, err := chain.BlockByHeightOnBranch(a3.Hash(), a2.Height())
	require.NoError(t, err)
	require.Equal(t, a2.Hash(), archived.Hash())
	_, err = chain.BlockHashByHeightOnBranch(a3.Hash(), a3.Height()+1)
	require.Error(t, err)
	_, err = chain.BlockHashByHeightOnBranch(a3.Hash(), -1)
	require.Error(t, err)
	_, err = chain.BlockHashByHeightOnBranch(&chainhash.Hash{}, 0)
	require.Error(t, err)
}
