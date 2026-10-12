package indexers

import (
	"cmp"
	"slices"
)

// ttlWrite retains the original deletion index even when disk writes are
// grouped by location. Ties sort by index so the last duplicate wins.
type ttlWrite struct {
	offset int64
	record flatFileRecord
	height int32
	index  uint32
}

// writeTTLBatch groups at most 1024 validated writes within record and 4 KiB
// page boundaries. The flat-file lock covers reads as well as writes, so
// unchanged bytes cannot race another writer. No buffer survives this call.
func (ff *FlatFileState) writeTTLBatch(height int32, writes []ttlWrite) error {
	slices.SortFunc(writes, func(a, b ttlWrite) int {
		if order := cmp.Compare(a.offset, b.offset); order != 0 {
			return order
		}
		return cmp.Compare(a.index, b.index)
	})
	var buf [4096]byte
	for i := 0; i < len(writes); {
		first := writes[i]
		end := i + 1
		contiguous := true
		for end < len(writes) && writes[end].height == first.height &&
			writes[end].offset/4096 == first.offset/4096 {
			if writes[end].offset > writes[end-1].offset+ttlSize {
				contiguous = false
			}
			end++
		}
		// Sparse pairs would replace two small writes with a read plus a
		// larger write. Only stage gaps when at least three writes share a page.
		if !contiguous && end-i < 3 {
			end = i + 1
			contiguous = true
		}
		size := int(writes[end-1].offset-first.offset) + ttlSize
		if !contiguous {
			if _, err := ff.dataFile.ReadAt(buf[:size], first.offset); err != nil {
				return err
			}
		}
		for _, w := range writes[i:end] {
			offset := int(w.offset - first.offset)
			byteOrder.PutUint32(buf[offset:offset+4], uint32(height))
			byteOrder.PutUint32(buf[offset+4:offset+ttlSize], w.index)
		}
		if err := ff.overwriteRecordLocked(first.height, first.record,
			int32(first.offset-first.record.offset), buf[:size]); err != nil {
			return err
		}
		i = end
	}
	return nil
}
