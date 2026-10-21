package bip183

import (
	"bytes"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/utreexo/utreexo"
	"github.com/utreexo/utreexod/bip182"
)

func BenchmarkProofHashesSerialize(b *testing.B) {
	hashes := make([]utreexo.Hash, 20000)
	var buf bytes.Buffer
	buf.Grow(len(hashes)*32 + 9)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buf.Reset()
		if err := ProofHashesSerialize(&buf, hashes); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashesFromLeafDatas(b *testing.B) {
	leaves := make([]bip182.LeafData, 5000)
	for i := range leaves {
		leaves[i] = bip182.LeafData{
			BlockHash: [32]byte{1},
			OutPoint:  wire.OutPoint{Hash: [32]byte{2}, Index: uint32(i)},
			Height:    500000,
			Amount:    100000,
			PkScript:  make([]byte, 25),
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := HashesFromLeafDatas(leaves); err != nil {
			b.Fatal(err)
		}
	}
}
