package store

import (
	"fmt"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// bitmapAdd adds entityID to the in-block bitmap cache for (key, val).
func bitmapAdd(cs *CacheStore, key, val string, entityID uint64) error {
	return mutateBitmap(cs, key, val, func(bm *roaring64.Bitmap) {
		bm.Add(entityID)
	})
}

// bitmapRemove removes entityID from the in-block bitmap cache for (key, val).
func bitmapRemove(cs *CacheStore, key, val string, entityID uint64) error {
	return mutateBitmap(cs, key, val, func(bm *roaring64.Bitmap) {
		bm.Remove(entityID)
	})
}

// mutateBitmap loads the bitmap for (key, val) into the per-block dirty cache
// on first touch, journals the pre-block pointer value, then applies mutate.
// The bitmap is not written to stagingDB until flushBitmaps() is called at
// Commit time, so each annotation produces exactly one blob per block.
func mutateBitmap(cs *CacheStore, key, val string, mutate func(*roaring64.Bitmap)) error {
	pair := annotPair{key, val}

	if _, cached := cs.dirtyBitmaps[pair]; !cached {
		// First touch: read pre-block pointer and journal it for revert.
		oldHashBytes, _ := cs.stagingDB.Get(annotKey(key, val))
		cs.journal.record(annotKey(key, val), oldHashBytes)

		// Load the existing bitmap, or start fresh if this annotation is new.
		bm := roaring64.New()
		if len(oldHashBytes) == common.HashLength {
			bmBytes, err := cs.stagingDB.Get(bitmapKey(common.BytesToHash(oldHashBytes)))
			if err != nil {
				return err
			}
			if err := bm.UnmarshalBinary(bmBytes); err != nil {
				return err
			}
		}
		cs.dirtyBitmaps[pair] = bm
	}

	mutate(cs.dirtyBitmaps[pair])
	return nil
}

// flushBitmaps serialises every dirty bitmap, writes one content-addressed blob
// and one pointer update per annotation to stagingDB, and updates the trie slot.
// Called once per block from CacheStore.Commit(), before stateDB.Commit().
func (c *CacheStore) flushBitmaps() error {
	for pair, bm := range c.dirtyBitmaps {
		newBytes, err := bm.MarshalBinary()
		if err != nil {
			return fmt.Errorf("marshal bitmap (%s, %s): %w", pair.key, pair.val, err)
		}
		newHash := crypto.Keccak256Hash(newBytes)

		if err := c.stagingDB.Put(bitmapKey(newHash), newBytes); err != nil {
			return err
		}
		if err := c.stagingDB.Put(annotKey(pair.key, pair.val), newHash.Bytes()); err != nil {
			return err
		}
		setAnnotSlot(c.stateDB, pair.key, pair.val, newHash)

		// Existence index: written once per annotation pair ever seen.
		pk := pairsKey(pair.key, pair.val)
		if has, _ := c.stagingDB.Has(pk); !has {
			if err := c.stagingDB.Put(pk, []byte{0x01}); err != nil {
				return err
			}
		}
	}
	return nil
}
