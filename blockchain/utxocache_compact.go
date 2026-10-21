package blockchain

import (
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/utreexo/utreexod/blockchain/internal/sizehelper"
)

// compactIfSparse releases storage retained by fresh-output churn before it
// forces a full flush. Require substantial churn and enough savings to leave
// headroom; rebuilding a dense map immediately before flushing is wasted work.
// The chain write lock protects entries/flags and cache accounting throughout.
func (s *utxoCache) compactIfSparse() bool {
	m := &s.cachedEntries
	m.mtx.Lock()
	defer m.mtx.Unlock()
	live := uint64(len(m.entries))
	if m.insertions < 1<<20 || m.insertions < 2*live {
		return false
	}
	// At most one dirty descriptor per live entry. Leave ample allowance
	// for allocator rounding while preserving the conservative map charge.
	projected := 512 + live*(utxoMapInsertionSize+uint64(sizehelper.OutpointSize)) + s.totalEntryMemory
	if projected > s.maxTotalMemoryUsage/4*3 {
		return false
	}
	entries := make(map[wire.OutPoint]*UtxoEntry, len(m.entries))
	dirty := make([]wire.OutPoint, 0, len(m.entries))
	for op, entry := range m.entries {
		entries[op] = entry
		if entry != nil && entry.isModified() {
			dirty = append(dirty, op)
		}
	}
	// Entry pointers and flags are unchanged, including nil misses and spent
	// persisted entries. Only a real map replacement resets insertion charges.
	m.entries, m.insertions = entries, live
	s.dirtyOutpoints = dirty
	return true
}
