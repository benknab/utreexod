package txscript

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/utreexo/utreexod/wire"
)

// Retain the original reconstruct-and-compare path as an independent oracle
// and benchmark baseline for the direct payload comparison.
func reconstructScriptTypeReference(sig, pk []byte, witness wire.TxWitness) (ScriptClass, error) {
	class := GetScriptClass(pk)
	switch class {
	case PubKeyHashTy, ScriptHashTy:
		if len(sig) == 0 {
			return NonStandardTy, fmt.Errorf("Have %v script but sigscript is %v", class.String(), sig)
		}
	case WitnessV0PubKeyHashTy, WitnessV0ScriptHashTy:
		if len(witness) == 0 {
			return NonStandardTy, fmt.Errorf("Have %v script but witness is %v", class.String(), witness)
		}
	default:
		return NonStandardTy, nil
	}
	script, err := ReconstructScript(sig, witness, class)
	if err != nil {
		return NonStandardTy, err
	}
	if !bytes.Equal(script, pk) {
		return NonStandardTy, nil
	}
	return class, nil
}

func checkReconstructParity(t *testing.T, sig, pk []byte, witness wire.TxWitness) {
	t.Helper()
	want, wantErr := reconstructScriptTypeReference(sig, pk, witness)
	got, err := GetReconstructScriptType(sig, pk, witness)
	if got != want || fmt.Sprint(err) != fmt.Sprint(wantErr) {
		t.Fatalf("sig=%x pk=%x witness=%x: got (%v, %v), want (%v, %v)",
			sig, pk, witness, got, err, want, wantErr)
	}
}

func TestReconstructScriptTypeParity(t *testing.T) {
	sigs := [][]byte{
		nil, {OP_0}, {OP_1}, {OP_DATA_1, 42},
		{OP_PUSHDATA1, 1, 42}, {OP_PUSHDATA1, 0},
		{OP_DATA_1, 42, OP_DROP}, {OP_DATA_1, 42, OP_DATA_2, 1},
		{OP_PUSHDATA4, 255, 255, 255, 255},
	}
	witnesses := []wire.TxWitness{nil, {nil}, {{42}}, {{1, 2}, {3, 4, 5}}}
	for _, sig := range sigs {
		for _, witness := range witnesses {
			for _, class := range []ScriptClass{PubKeyHashTy, ScriptHashTy, WitnessV0PubKeyHashTy, WitnessV0ScriptHashTy} {
				pk, err := ReconstructScript(sig, witness, class)
				if err != nil {
					pk, _ = ReconstructScript(sig, wire.TxWitness{{42}}, class)
				}
				checkReconstructParity(t, sig, pk, witness)
				pk[len(pk)/2] ^= 1
				checkReconstructParity(t, sig, pk, witness)
			}
			checkReconstructParity(t, sig, []byte{OP_TRUE}, witness)
		}
	}
}

func FuzzReconstructScriptTypeParity(f *testing.F) {
	f.Add([]byte{OP_DATA_1, 42}, []byte{42}, []byte{OP_TRUE})
	f.Add([]byte{OP_DATA_1, 42, OP_DROP}, []byte{}, []byte{})
	f.Add([]byte{OP_PUSHDATA1, 3, 1}, []byte{1, 2, 3}, []byte{OP_HASH160})
	f.Fuzz(func(t *testing.T, sig, last, arbitraryPk []byte) {
		if len(sig)+len(last)+len(arbitraryPk) > 3*MaxScriptSize {
			t.Skip()
		}
		witness := wire.TxWitness{{1}, last}
		checkReconstructParity(t, sig, arbitraryPk, witness)
		for _, class := range []ScriptClass{PubKeyHashTy, ScriptHashTy, WitnessV0PubKeyHashTy, WitnessV0ScriptHashTy} {
			pk, err := ReconstructScript(sig, witness, class)
			if err != nil {
				t.Fatal(err)
			}
			checkReconstructParity(t, sig, pk, witness)
			pk[len(pk)/2] ^= 1
			checkReconstructParity(t, sig, pk, witness)
		}
	})
}

func BenchmarkReconstructScriptType(b *testing.B) {
	key := append([]byte{2}, bytes.Repeat([]byte{42}, 32)...)
	sig, _ := NewScriptBuilder().AddData(bytes.Repeat([]byte{1}, 72)).AddData(key).Script()
	witness := wire.TxWitness{bytes.Repeat([]byte{1}, 72), key}
	for _, class := range []ScriptClass{PubKeyHashTy, ScriptHashTy, WitnessV0PubKeyHashTy, WitnessV0ScriptHashTy} {
		pk, err := ReconstructScript(sig, witness, class)
		if err != nil {
			b.Fatal(err)
		}
		for _, impl := range []struct {
			name string
			fn   func([]byte, []byte, wire.TxWitness) (ScriptClass, error)
		}{{"reference", reconstructScriptTypeReference}, {"direct", GetReconstructScriptType}} {
			b.Run(class.String()+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					got, err := impl.fn(sig, pk, witness)
					if err != nil || got != class {
						b.Fatalf("got %v, %v", got, err)
					}
				}
			})
		}
	}
}
