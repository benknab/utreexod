package blockchain

import (
	"fmt"
	"math"
	"testing"

	"github.com/btcsuite/btcd/wire/v2"
	"github.com/utreexo/utreexod/btcutil"
	"github.com/utreexo/utreexod/txscript"
)

func sanityBenchmarkTx(inputs int) *btcutil.Tx {
	tx := wire.NewMsgTx(2)
	for i := range inputs {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: uint32(i)}})
	}
	tx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	return btcutil.NewTx(tx)
}

func BenchmarkTransactionSanity(b *testing.B) {
	for _, inputs := range []int{1, 2, 8, 100, 5000} {
		b.Run(fmt.Sprint(inputs), func(b *testing.B) {
			tx := sanityBenchmarkTx(inputs)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := CheckTransactionSanity(tx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestTransactionSanityDuplicateInputs(t *testing.T) {
	for _, inputs := range []int{1, 2, 3, 8, 100} {
		tx := sanityBenchmarkTx(inputs)
		if err := CheckTransactionSanity(tx); err != nil {
			t.Fatal(err)
		}
		for i := 1; i < inputs; i++ {
			original := tx.MsgTx().TxIn[i].PreviousOutPoint
			tx.MsgTx().TxIn[i].PreviousOutPoint = tx.MsgTx().TxIn[0].PreviousOutPoint
			err, ok := CheckTransactionSanity(tx).(RuleError)
			if !ok || err.ErrorCode != ErrDuplicateTxInputs {
				t.Fatalf("inputs=%d duplicate=%d: %v", inputs, i, err)
			}
			tx.MsgTx().TxIn[i].PreviousOutPoint = original
		}
		// A null prevout remains forbidden in multi-input transactions; a
		// single null input must still undergo the coinbase script check.
		tx.MsgTx().TxIn[inputs-1].PreviousOutPoint = wire.OutPoint{Index: math.MaxUint32}
		want := ErrBadTxInput
		if inputs == 1 {
			want = ErrBadCoinbaseScriptLen
		}
		err, ok := CheckTransactionSanity(tx).(RuleError)
		if !ok || err.ErrorCode != want {
			t.Fatalf("inputs=%d null prevout: %v", inputs, err)
		}
		if inputs > 1 {
			tx.MsgTx().TxIn[0].PreviousOutPoint = tx.MsgTx().TxIn[inputs-1].PreviousOutPoint
			err, ok := CheckTransactionSanity(tx).(RuleError)
			if !ok || err.ErrorCode != ErrDuplicateTxInputs {
				t.Fatalf("duplicate null prevouts must retain error precedence: %v", err)
			}
		}
	}
}
