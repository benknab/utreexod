package txscript

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/utreexo/utreexod/chaincfg/chainhash"
	"github.com/utreexo/utreexod/wire"
)

func hashCacheBenchmarkTx(inputs int, taproot bool) (*wire.MsgTx, PrevOutputFetcher) {
	tx := wire.NewMsgTx(2)
	for i := range inputs {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i)}, Sequence: uint32(i + 1)})
	}
	script := append([]byte{OP_0, OP_DATA_20}, make([]byte, 20)...)
	if taproot {
		script = append([]byte{OP_1, OP_DATA_32}, make([]byte, 32)...)
	}
	for range 2 {
		tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: script})
	}
	return tx, NewCannedPrevOutputFetcher(script, 123456)
}

func BenchmarkTxSigHashes(b *testing.B) {
	for _, taproot := range []bool{false, true} {
		for _, inputs := range []int{1, 2, 100, 5000} {
			b.Run(fmt.Sprintf("taproot=%t/%d", taproot, inputs), func(b *testing.B) {
				tx, fetcher := hashCacheBenchmarkTx(inputs, taproot)
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					NewTxSigHashes(tx, fetcher)
				}
			})
		}
	}
}

// Independently serialize the BIP143/BIP341 preimages, including CompactSize
// boundaries and mixed input types, to protect the streaming implementation.
func TestSigHashMidstatePreimages(t *testing.T) {
	for _, inputs := range []int{1, 2, 17, 253} {
		for _, scriptLen := range []int{0, 22, 34, 252, 253, 65535, 65536} {
			tx, _ := hashCacheBenchmarkTx(inputs, false)
			fetcher := NewMultiPrevOutFetcher(nil)
			var prevouts, sequences, outputs, amounts, scripts bytes.Buffer
			for i, in := range tx.TxIn {
				in.PreviousOutPoint.Hash[0] = byte(i + 1)
				prevouts.Write(in.PreviousOutPoint.Hash[:])
				binary.Write(&prevouts, binary.LittleEndian, in.PreviousOutPoint.Index)
				binary.Write(&sequences, binary.LittleEndian, in.Sequence)
				script := bytes.Repeat([]byte{OP_TRUE}, scriptLen)
				// Ensure a taproot input; with multiple inputs this is mixed.
				if i == 0 {
					script = append([]byte{OP_1, OP_DATA_32}, make([]byte, 32)...)
				}
				value := int64(i)*123456 - 1
				fetcher.AddPrevOut(in.PreviousOutPoint, &wire.TxOut{Value: value, PkScript: script})
				binary.Write(&amounts, binary.LittleEndian, value)
				wire.WriteVarBytes(&scripts, 0, script)
			}
			tx.TxOut[0].PkScript = bytes.Repeat([]byte{OP_TRUE}, scriptLen)
			for _, out := range tx.TxOut {
				wire.WriteTxOut(&outputs, 0, 0, out)
			}
			got := NewTxSigHashes(tx, fetcher)
			want := TxSigHashes{TaprootSigHashMidState: TaprootSigHashMidState{
				HashPrevOutsV1:     chainhash.HashH(prevouts.Bytes()),
				HashSequenceV1:     chainhash.HashH(sequences.Bytes()),
				HashOutputsV1:      chainhash.HashH(outputs.Bytes()),
				HashInputAmountsV1: chainhash.HashH(amounts.Bytes()),
				HashInputScriptsV1: chainhash.HashH(scripts.Bytes()),
			}}
			if inputs > 1 {
				want.HashPrevOutsV0 = chainhash.DoubleHashH(prevouts.Bytes())
				want.HashSequenceV0 = chainhash.DoubleHashH(sequences.Bytes())
				want.HashOutputsV0 = chainhash.DoubleHashH(outputs.Bytes())
			}
			if *got != want {
				t.Fatalf("inputs=%d scriptLen=%d: got %+v, want %+v", inputs, scriptLen, *got, want)
			}
		}
	}
}

func TestSigHashMidstateConcurrent(t *testing.T) {
	for worker := range 16 {
		t.Run(fmt.Sprint(worker), func(t *testing.T) {
			t.Parallel()
			tx, fetcher := hashCacheBenchmarkTx(worker+1, worker%2 == 0)
			want := *NewTxSigHashes(tx, fetcher)
			for range 100 {
				got := NewTxSigHashes(tx, fetcher)
				if *got != want {
					t.Fatal("concurrent workspace reuse changed sighash midstates")
				}
			}
		})
	}
}
