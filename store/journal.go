package store

import (
	"encoding/binary"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/rlp"
)

// journalEntry records the previous value of a mutable PebbleDB key so it can
// be restored on reorg. OldValue is nil when the key did not exist before.
type journalEntry struct {
	Key      []byte
	OldValue []byte
}

// blockJournal accumulates mutable PebbleDB changes during a single block.
type blockJournal struct {
	entries []journalEntry
}

// record saves the old value of a key before it is overwritten or created.
// It must be called before every mutable PebbleDB write.
func (j *blockJournal) record(key, oldValue []byte) {
	j.entries = append(j.entries, journalEntry{Key: key, OldValue: oldValue})
}

// persist writes all accumulated journal entries for a block to PebbleDB.
// Called at the end of ProcessBlock, after StateDB.Commit.
func (j *blockJournal) persist(db ethdb.Database, blockNumber uint64, blockHash common.Hash) error {
	if len(j.entries) == 0 {
		return nil
	}
	batch := db.NewBatch()
	for i, entry := range j.entries {
		data, err := rlp.EncodeToBytes(entry)
		if err != nil {
			return err
		}
		if err := batch.Put(journalKey(blockNumber, blockHash, uint32(i)), data); err != nil {
			return err
		}
	}
	return batch.Write()
}

// revertBlockJournal reads all journal entries for a block and replays them in
// reverse, restoring mutable PebbleDB keys to their pre-block state.
func revertBlockJournal(db ethdb.Database, blockNumber uint64, blockHash common.Hash) error {
	prefix := journalPrefix(blockNumber, blockHash)

	type indexed struct {
		idx   uint32
		entry journalEntry
	}
	var entries []indexed

	it := db.NewIterator(prefix, nil)
	defer it.Release()
	for it.Next() {
		key := it.Key()
		// Last 4 bytes of the journal key are the entry index.
		idx := binary.BigEndian.Uint32(key[len(key)-4:])
		var e journalEntry
		if err := rlp.DecodeBytes(it.Value(), &e); err != nil {
			return err
		}
		entries = append(entries, indexed{idx, e})
	}
	if err := it.Error(); err != nil {
		return err
	}

	// Sort ascending by index so we can reverse-iterate.
	sort.Slice(entries, func(i, j int) bool { return entries[i].idx < entries[j].idx })

	batch := db.NewBatch()

	// Replay in reverse order.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i].entry
		if e.OldValue == nil {
			if err := batch.Delete(e.Key); err != nil {
				return err
			}
		} else {
			if err := batch.Put(e.Key, e.OldValue); err != nil {
				return err
			}
		}
	}

	// Delete journal entries for this block.
	for _, e := range entries {
		k := journalKey(blockNumber, blockHash, e.idx)
		if err := batch.Delete(k); err != nil {
			return err
		}
	}

	return batch.Write()
}
