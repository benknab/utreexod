package wire

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"sync"
	"testing"

	"github.com/utreexo/utreexod/chaincfg/chainhash"
)

func hashFixture(inputs int) *MsgTx {
	tx := NewMsgTx(2)
	for i := range inputs {
		tx.AddTxIn(&TxIn{PreviousOutPoint: OutPoint{Index: uint32(i)},
			Sequence: uint32(i), SignatureScript: bytes.Repeat([]byte{byte(i)}, i%100),
			Witness: TxWitness{bytes.Repeat([]byte{byte(i)}, 72), bytes.Repeat([]byte{3}, 33)}})
		tx.AddTxOut(&TxOut{Value: int64(i + 1), PkScript: bytes.Repeat([]byte{0x51}, 25)})
	}
	return tx
}

func checkTransactionHashes(t *testing.T, tx *MsgTx) {
	t.Helper()
	var plain, witness bytes.Buffer
	if err := tx.SerializeNoWitness(&plain); err != nil {
		t.Fatal(err)
	}
	if err := tx.Serialize(&witness); err != nil {
		t.Fatal(err)
	}
	if got, want := tx.TxHash(), chainhash.DoubleHashH(plain.Bytes()); got != want {
		t.Fatalf("txid %v != %v", got, want)
	}
	if got, want := tx.WitnessHash(), chainhash.DoubleHashH(witness.Bytes()); got != want {
		t.Fatalf("wtxid %v != %v", got, want)
	}
}

func TestTransactionHashPool(t *testing.T) {
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := range 100 {
				tx := hashFixture((i+n)%32 + 1)
				checkTransactionHashes(t, tx)
				for _, in := range tx.TxIn {
					in.Witness = nil
				}
				checkTransactionHashes(t, tx)
			}
		}()
	}
	wg.Wait()
}

func FuzzTransactionHashSerialization(f *testing.F) {
	var buf bytes.Buffer
	_ = hashFixture(3).Serialize(&buf)
	f.Add(buf.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 100000 {
			return
		}
		var tx MsgTx
		if err := tx.Deserialize(bytes.NewReader(data)); err != nil {
			return
		}
		checkTransactionHashes(t, &tx)
	})
}

func BenchmarkTransactionHashStorage(b *testing.B) {
	for _, n := range []int{1, 64} {
		for _, witness := range []bool{false, true} {
			for _, legacy := range []bool{true, false} {
				b.Run(fmt.Sprintf("inputs=%d/witness=%t/legacy=%t", n, witness, legacy), func(b *testing.B) {
					tx := hashFixture(n)
					b.ReportAllocs()
					b.ResetTimer()
					for range b.N {
						if legacy {
							if witness {
								buf := bytes.NewBuffer(make([]byte, 0, tx.SerializeSize()))
								_ = tx.Serialize(buf)
								_ = chainhash.DoubleHashH(buf.Bytes())
							} else {
								h := sha256.New()
								_ = tx.SerializeNoWitness(h)
								_ = chainhash.DoubleHashRaw(h)
							}
						} else if witness {
							_ = tx.WitnessHash()
						} else {
							_ = tx.TxHash()
						}
					}
				})
			}
		}
	}
}
