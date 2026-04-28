package store

import (
	"bytes"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/ethdb"
)

// repopulatePebbleDB writes corrected values for all mutable PebbleDB entries
// (arkiv_annot, arkiv_id, arkiv_addr) into batch, derived from the trie state
// in sdb (already opened at the reverted state root).
//
// This is the reorg reversion mechanism. The trie reverts for free via
// HashScheme; the mutable PebbleDB entries are not trie-versioned and must be
// brought back into sync by reading the authoritative values from the trie.
// Reorg is rare, so the full scan cost is acceptable.
func repopulatePebbleDB(rawDB ethdb.Database, batch ethdb.Batch, sdb *state.StateDB) error {
	if err := repopulateAnnot(rawDB, batch, sdb); err != nil {
		return err
	}
	return repopulateIDs(rawDB, batch, sdb)
}

// repopulateAnnot scans all arkiv_pairs entries and writes the correct
// arkiv_annot pointer for each (annotKey, annotVal) pair by reading the
// system account trie slot at the reverted state root.
// Pairs whose slot is now zero (first introduced in a reverted block) have
// their arkiv_annot entry deleted.
func repopulateAnnot(rawDB ethdb.Database, batch ethdb.Batch, sdb *state.StateDB) error {
	it := rawDB.NewIterator(prefixPairs, nil)
	defer it.Release()
	for it.Next() {
		suffix := it.Key()[len(prefixPairs):]
		sep := bytes.IndexByte(suffix, 0x00)
		if sep < 0 {
			continue // malformed key; skip
		}
		aKey := string(suffix[:sep])
		aVal := string(suffix[sep+1:])

		hash := sdb.GetState(systemAddress, annotSlotKey(aKey, aVal))
		ak := annotKey(aKey, aVal)
		if hash == (common.Hash{}) {
			if err := batch.Delete(ak); err != nil {
				return err
			}
		} else {
			if err := batch.Put(ak, hash.Bytes()); err != nil {
				return err
			}
		}
	}
	return it.Error()
}

// repopulateIDs scans all arkiv_id entries and removes any whose system account
// trie slot is now zero — meaning the entity was first created in a reverted
// block and should not exist at the reverted state root.
// Entries whose slot is non-zero are left in place: either the entity still
// exists, or the entry is a valid tombstone from a pre-revert delete.
func repopulateIDs(rawDB ethdb.Database, batch ethdb.Batch, sdb *state.StateDB) error {
	it := rawDB.NewIterator(prefixID, nil)
	defer it.Release()
	for it.Next() {
		if len(it.Key()) != len(prefixID)+8 {
			continue // unexpected key length; skip
		}
		id := decodeUint64(it.Key()[len(prefixID):])
		slotVal := sdb.GetState(systemAddress, idSlotKey(id))
		if slotVal == (common.Hash{}) {
			// Entity was created in a reverted block; remove both cache entries.
			if len(it.Value()) == 20 {
				addr := common.BytesToAddress(it.Value())
				if err := batch.Delete(addrKey(addr)); err != nil {
					return err
				}
			}
			keyCopy := make([]byte, len(it.Key()))
			copy(keyCopy, it.Key())
			if err := batch.Delete(keyCopy); err != nil {
				return err
			}
		}
	}
	return it.Error()
}
