package indexers

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
	"github.com/utreexo/utreexod/database"
)

type addrOrderPage struct {
	Regions []database.BlockRegion
	Skipped uint32
}

type addrOrderState struct {
	Entries map[string][]byte
	Hash    chainhash.Hash
	Height  int32
	Pages   map[string]addrOrderPage
}

var errAddrOrderWrite = errors.New("injected address write failure")

type addrOrderBucket interface{ database.Bucket }

type addrOrderFailBucket struct {
	addrOrderBucket
	remaining *int
}

func (b addrOrderFailBucket) Bucket(key []byte) database.Bucket {
	return addrOrderFailBucket{addrOrderBucket: b.addrOrderBucket.Bucket(key), remaining: b.remaining}
}

func (b addrOrderFailBucket) Put(key, value []byte) error {
	if *b.remaining == 0 {
		return errAddrOrderWrite
	}
	*b.remaining--
	return b.addrOrderBucket.Put(key, value)
}

type addrOrderFailTx struct {
	database.Tx
	remaining int
}

func (tx *addrOrderFailTx) Metadata() database.Bucket {
	return addrOrderFailBucket{addrOrderBucket: tx.Tx.Metadata(), remaining: &tx.remaining}
}

// Compare persisted bytes and queries against the old map loop, including
// multi-address rollover, pinned snapshots, rollback, disconnect and reopen.
func TestAddrReadOrderParity(t *testing.T) {
	var keys [][addrKeySize]byte
	histories := []int{0, 1, 7, 8, 9, 16, 17, 24, 25, 56, 57, 120}
	for typ := byte(0); typ <= addrKeyTypeTaprootPubKey; typ++ {
		for i := range histories {
			var key [addrKeySize]byte
			key[0], key[addrKeySize-1] = typ, byte(i)
			keys = append(keys, key) // Adjacent hashes, shared hashes across types.
		}
	}
	locs := make([]wire.TxLoc, 128)
	for i := range locs {
		locs[i] = wire.TxLoc{TxStart: 81 + i*300, TxLen: 200 + i}
	}
	data := make(writeIndexData)
	for _, key := range keys {
		data[key] = []int{0, 2, 5, 9}
	}
	capture := func(t *testing.T, tx database.Tx) addrOrderState {
		s := addrOrderState{Entries: make(map[string][]byte), Pages: make(map[string]addrOrderPage)}
		require.NoError(t, tx.Metadata().Bucket(addrIndexKey).ForEach(func(key, value []byte) error {
			s.Entries[string(key)] = bytes.Clone(value)
			return nil
		}))
		hash, height, err := dbFetchIndexerTip(tx, addrIndexKey)
		require.NoError(t, err)
		s.Hash, s.Height = *hash, height
		for _, key := range keys {
			for _, reverse := range []bool{false, true} {
				for _, skip := range []uint32{0, 1, 7, 1000} {
					for _, count := range []uint32{0, 1, 8, 1000} {
						regions, skipped, err := dbFetchAddrIndexEntries(tx, key, skip, count, reverse)
						require.NoError(t, err)
						s.Pages[fmt.Sprintf("%x/%v/%d/%d", key, reverse, skip, count)] = addrOrderPage{regions, skipped}
					}
				}
			}
		}
		return s
	}
	var reference []addrOrderState
	for _, mode := range []struct {
		name  string
		apply func(database.Tx, uint32, []wire.TxLoc, writeIndexData) error
	}{{"map", connectAddrEntriesUnsorted}, {"sorted", connectAddrEntries}} {
		t.Run(mode.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "db")
			db, err := database.Create("ffldb", path, wire.MainNet)
			require.NoError(t, err)
			defer func() { require.NoError(t, db.Close()) }()
			require.NoError(t, db.Update(func(tx database.Tx) error {
				for _, key := range [][]byte{addrIndexKey, idByHashIndexBucketName, hashByIDIndexBucketName, indexTipsBucketName} {
					_, err := tx.Metadata().CreateBucket(key)
					require.NoError(t, err)
				}
				for id := uint32(1); id <= 6; id++ {
					require.NoError(t, dbPutBlockIDIndexEntry(tx, &chainhash.Hash{byte(id)}, id))
				}
				bucket := tx.Metadata().Bucket(addrIndexKey)
				for i, key := range keys {
					for n := 0; n < histories[i%len(histories)]; n++ {
						require.NoError(t, dbPutAddrIndexEntry(bucket, key, 1, locs[n]))
					}
				}
				return dbPutIndexerTip(tx, addrIndexKey, &chainhash.Hash{1}, 1)
			}))
			var states []addrOrderState
			snapshot := func() addrOrderState {
				var state addrOrderState
				require.NoError(t, db.View(func(tx database.Tx) error { state = capture(t, tx); return nil }))
				return state
			}
			apply := func(id uint32, data writeIndexData) {
				require.NoError(t, db.Update(func(tx database.Tx) error {
					if err := mode.apply(tx, id, locs, data); err != nil {
						return err
					}
					return dbPutIndexerTip(tx, addrIndexKey, &chainhash.Hash{byte(id)}, int32(id))
				}))
				states = append(states, snapshot())
			}
			apply(2, nil)
			apply(3, writeIndexData{})
			apply(4, writeIndexData{keys[0]: {0}})
			apply(5, data)
			pinned, err := db.Begin(false)
			require.NoError(t, err)
			apply(6, data)
			require.Equal(t, states[len(states)-2], capture(t, pinned))
			require.NoError(t, pinned.Rollback())
			require.NoError(t, db.Close())
			db, err = database.Open("ffldb", path, wire.MainNet)
			require.NoError(t, err)
			require.Equal(t, states[len(states)-1], snapshot())
			require.ErrorIs(t, db.Update(func(tx database.Tx) error {
				return mode.apply(&addrOrderFailTx{Tx: tx, remaining: 5}, 6, locs, data)
			}), errAddrOrderWrite)
			require.Equal(t, states[len(states)-1], snapshot())
			require.NoError(t, db.Update(func(tx database.Tx) error {
				if err := disconnectAddrEntries(tx, data); err != nil {
					return err
				}
				return dbPutIndexerTip(tx, addrIndexKey, &chainhash.Hash{5}, 5)
			}))
			require.Equal(t, states[len(states)-2], snapshot())
			apply(6, data)
			require.Equal(t, states[len(states)-2], states[len(states)-1])
			if mode.name == "map" {
				reference = states
			} else {
				require.Equal(t, reference, states)
			}
		})
	}
}
