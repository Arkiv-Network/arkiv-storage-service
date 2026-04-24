package store

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/ethdb"
)

func checkValue(t *testing.T, db ethdb.Database, key, want []byte) {
	t.Helper()
	got, err := db.Get(key)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if string(got) != string(want) {
		t.Errorf("key %q: got %q, want %q", key, got, want)
	}
}

// TestJournalRoundTrip verifies that persist followed by revertBlockJournal
// restores all keys to their pre-block values and deletes the journal entries.
func TestJournalRoundTrip(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	blockNum := uint64(42)
	blockHash := common.HexToHash("0xabcdef")

	// Establish initial state: A and B exist, C does not.
	_ = db.Put([]byte("key_A"), []byte("old_A"))
	_ = db.Put([]byte("key_B"), []byte("old_B"))

	// Record before-values, then apply new state.
	j := &blockJournal{}
	j.record([]byte("key_A"), []byte("old_A"))
	j.record([]byte("key_B"), []byte("old_B"))
	j.record([]byte("key_C"), nil) // key_C was absent

	_ = db.Put([]byte("key_A"), []byte("new_A"))
	_ = db.Put([]byte("key_B"), []byte("new_B"))
	_ = db.Put([]byte("key_C"), []byte("new_C"))

	if err := j.persist(db, blockNum, blockHash); err != nil {
		t.Fatalf("persist: %v", err)
	}

	// Verify journal entries were written.
	prefix := journalPrefix(blockNum, blockHash)
	it := db.NewIterator(prefix, nil)
	count := 0
	for it.Next() {
		count++
	}
	it.Release()
	if count != 3 {
		t.Errorf("expected 3 journal entries, got %d", count)
	}

	// Revert.
	batch := db.NewBatch()
	if err := revertBlockJournal(db, batch, blockNum, blockHash); err != nil {
		t.Fatalf("revertBlockJournal: %v", err)
	}
	if err := batch.Write(); err != nil {
		t.Fatalf("batch.Write: %v", err)
	}

	// Keys A and B are restored to old values.
	checkValue(t, db, []byte("key_A"), []byte("old_A"))
	checkValue(t, db, []byte("key_B"), []byte("old_B"))

	// Key C is deleted (old value was nil).
	if has, _ := db.Has([]byte("key_C")); has {
		t.Error("key_C should be absent after revert (old value was nil)")
	}

	// Journal entries are cleaned up.
	it = db.NewIterator(prefix, nil)
	defer it.Release()
	if it.Next() {
		t.Error("journal entries still present after revert")
	}
}

// TestJournalRevertOrder verifies that when the same key is recorded twice in
// a single block journal, reverse-order replay restores the state from before
// the first write — not the intermediate value.
func TestJournalRevertOrder(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	blockNum := uint64(1)
	blockHash := common.HexToHash("0x1234")

	key := []byte("shared_key")

	j := &blockJournal{}

	// First write: key was absent.
	j.record(key, nil)
	_ = db.Put(key, []byte("v1"))

	// Second write: key is now v1.
	j.record(key, []byte("v1"))
	_ = db.Put(key, []byte("v2"))

	if err := j.persist(db, blockNum, blockHash); err != nil {
		t.Fatalf("persist: %v", err)
	}
	batch2 := db.NewBatch()
	if err := revertBlockJournal(db, batch2, blockNum, blockHash); err != nil {
		t.Fatalf("revertBlockJournal: %v", err)
	}
	if err := batch2.Write(); err != nil {
		t.Fatalf("batch2.Write: %v", err)
	}

	// Reverse replay: entry[1] restores key=v1, then entry[0] deletes key.
	// The net result must be the state before the first write: absent.
	if has, _ := db.Has(key); has {
		v, _ := db.Get(key)
		t.Errorf("key should be absent after full revert, got %q", v)
	}
}
