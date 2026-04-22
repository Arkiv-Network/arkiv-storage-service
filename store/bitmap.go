package store

import (
	"github.com/RoaringBitmap/roaring/v2/roaring64"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethdb"
)

// bitmapAdd adds entityID to the bitmap for (key, val) and journals the change.
func bitmapAdd(db ethdb.Database, sdb *state.StateDB, j *blockJournal, key, val string, entityID uint64) error {
	return mutateBitmap(db, sdb, j, key, val, func(bm *roaring64.Bitmap) {
		bm.Add(entityID)
	})
}

// bitmapRemove removes entityID from the bitmap for (key, val) and journals the change.
func bitmapRemove(db ethdb.Database, sdb *state.StateDB, j *blockJournal, key, val string, entityID uint64) error {
	return mutateBitmap(db, sdb, j, key, val, func(bm *roaring64.Bitmap) {
		bm.Remove(entityID)
	})
}

func mutateBitmap(db ethdb.Database, sdb *state.StateDB, j *blockJournal, key, val string, mutate func(*roaring64.Bitmap)) error {
	pKey := annotKey(key, val)

	// Read the current pointer hash (nil if absent).
	oldHashBytes, _ := db.Get(pKey)

	// Journal the old pointer value before modifying it.
	j.record(pKey, oldHashBytes)

	// Load or create bitmap.
	bm := roaring64.New()
	if len(oldHashBytes) == common.HashLength {
		bmBytes, err := db.Get(bitmapKey(common.BytesToHash(oldHashBytes)))
		if err != nil {
			return err
		}
		if err := bm.UnmarshalBinary(bmBytes); err != nil {
			return err
		}
	}

	mutate(bm)

	// Serialize the new bitmap and compute its content-addressed key.
	newBytes, err := bm.MarshalBinary()
	if err != nil {
		return err
	}
	newHash := crypto.Keccak256Hash(newBytes)

	// Write new immutable bitmap entry (old entry is left in place).
	if err := db.Put(bitmapKey(newHash), newBytes); err != nil {
		return err
	}

	// Update mutable pointer.
	if err := db.Put(pKey, newHash.Bytes()); err != nil {
		return err
	}

	// Update trie-committed system account slot.
	setAnnotSlot(sdb, key, val, newHash)

	// Record in the append-only existence index (never reverted).
	pk := pairsKey(key, val)
	if has, _ := db.Has(pk); !has {
		if err := db.Put(pk, []byte{0x01}); err != nil {
			return err
		}
	}

	return nil
}
