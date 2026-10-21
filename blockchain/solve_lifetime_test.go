package blockchain

import (
	"runtime"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/chaincfg/v2"
)

func TestSolveBlockJoinsWorkers(t *testing.T) {
	stacks := func() int {
		buf := make([]byte, 1<<20)
		return strings.Count(string(buf[:runtime.Stack(buf, true)]), "blockchain.SolveBlock.func1(")
	}
	before := stacks()
	header := chaincfg.RegressionNetParams.GenesisBlock.Header
	for range 64 {
		if !SolveBlock(&header) {
			t.Fatal("regtest header not solved")
		}
		header.Timestamp = header.Timestamp.Add(1)
	}
	if after := stacks(); after != before {
		t.Fatalf("solver workers survived return: before=%d after=%d", before, after)
	}
}
