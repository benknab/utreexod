package blockchain

import (
	"fmt"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/txscript"
)

func scriptValidationBenchmarkTx(inputs int) *btcutil.Tx {
	tx := wire.NewMsgTx(2)
	for i := range inputs {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i)}})
	}
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	return btcutil.NewTx(tx)
}

func BenchmarkScriptValidation(b *testing.B) {
	for _, inputs := range []int{1, 5000} {
		b.Run(fmt.Sprint(inputs), func(b *testing.B) {
			tx := scriptValidationBenchmarkTx(inputs)
			view := NewUtxoViewpoint()
			for _, in := range tx.MsgTx().TxIn {
				view.entries[in.PreviousOutPoint] = NewUtxoEntry(
					&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}}, 1, false)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := ValidateTransactionScripts(tx, view, 0, nil, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestScriptValidationSerialAndParallel(t *testing.T) {
	for _, inputs := range []int{1, 2} {
		for _, tc := range []struct {
			name    string
			script  []byte
			missing bool
			code    ErrorCode
		}{
			{name: "valid", script: []byte{txscript.OP_TRUE}},
			{name: "missing", missing: true, code: ErrMissingTxOut},
			{name: "malformed", script: []byte{txscript.OP_PUSHDATA1}, code: ErrScriptMalformed},
			{name: "invalid", script: []byte{txscript.OP_FALSE}, code: ErrScriptValidation},
		} {
			t.Run(fmt.Sprintf("%d/%s", inputs, tc.name), func(t *testing.T) {
				tx := scriptValidationBenchmarkTx(inputs)
				tx.Hash()
				view := NewUtxoViewpoint()
				for _, in := range tx.MsgTx().TxIn {
					view.entries[in.PreviousOutPoint] = NewUtxoEntry(
						&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}}, 1, false)
				}
				op := tx.MsgTx().TxIn[inputs-1].PreviousOutPoint
				if tc.missing {
					delete(view.entries, op)
				} else {
					view.entries[op].pkScript = tc.script
				}
				err := ValidateTransactionScripts(tx, view, 0, nil, nil)
				if tc.name == "valid" {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				if ruleErr, ok := err.(RuleError); !ok || ruleErr.ErrorCode != tc.code {
					t.Fatalf("got %v, want %v", err, tc.code)
				}
			})
		}
	}
}
