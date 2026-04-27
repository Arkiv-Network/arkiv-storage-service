package store

import (
	"testing"

	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

// TestStagingOverlay_TwoOpsOnSameBitmap verifies that when two Create ops in the
// same block share an annotation, the second op reads the bitmap written by the
// first op from the staging layer — not from the (empty) real DB.
//
// This is the key correctness property of StagingDB: writes made by op[0] must
// be visible to op[1] within the same block.
func TestStagingOverlay_TwoOpsOnSameBitmap(t *testing.T) {
	s := NewMemory()

	noteStr := "note"
	block := makeBlock(1, testHash1, common.Hash{},
		types.ArkivOperation{Create: &types.CreateOp{
			EntityKey:   testKey1,
			Sender:      testSender,
			Owner:       testOwner1,
			Payload:     hexutil.Bytes("p1"),
			ContentType: "text/plain",
			ExpiresAt:   hexutil.Uint64(100),
			Annotations: []types.Annotation{{Key: "type", StringValue: &noteStr}},
		}},
		types.ArkivOperation{Create: &types.CreateOp{
			EntityKey:   testKey2,
			Sender:      testSender,
			Owner:       testOwner1,
			Payload:     hexutil.Bytes("p2"),
			ContentType: "text/plain",
			ExpiresAt:   hexutil.Uint64(100),
			Annotations: []types.Annotation{{Key: "type", StringValue: &noteStr}},
		}},
	)
	if _, err := s.ProcessBlock(block); err != nil {
		t.Fatalf("ProcessBlock: %v", err)
	}

	// Both entity IDs must be in the shared user annotation bitmap.
	bm := readBitmap(t, s, "type", "note")
	if !bm.Contains(1) {
		t.Error("type=note bitmap missing entity ID 1")
	}
	if !bm.Contains(2) {
		t.Error("type=note bitmap missing entity ID 2; staging overlay did not expose op[0]'s writes to op[1]")
	}

	// $all is also shared across all entities — same test via a built-in annotation.
	allBm := readBitmap(t, s, "$all", "true")
	if allBm.GetCardinality() != 2 {
		t.Errorf("$all bitmap cardinality = %d, want 2", allBm.GetCardinality())
	}
}

// TestCacheStore_DiscardOnFailure verifies that when an op fails mid-block, the
// CacheStore is discarded and the real DB contains no partial writes from that
// block — not even from operations that succeeded before the failure.
func TestCacheStore_DiscardOnFailure(t *testing.T) {
	s := NewMemory()

	// Op 0 succeeds (Create entity A); op 1 fails (Update on non-existent entity B).
	block := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100),
		types.ArkivOperation{Update: &types.UpdateOp{
			EntityKey:   testKey2, // not created yet → processUpdate returns error
			Payload:     hexutil.Bytes("new"),
			ContentType: "text/plain",
			ExpiresAt:   200,
		}},
	)

	_, err := s.ProcessBlock(block)
	if err == nil {
		t.Fatal("ProcessBlock: expected error from failed op, got nil")
	}

	// Real DB must have no trace of op 0's writes (CacheStore.Discard was called).
	if has, _ := s.rawDB.Has(idKey(1)); has {
		t.Error("idKey(1) present in rawDB after failed block — Discard did not work")
	}
	if has, _ := s.rawDB.Has(annotKey("$all", "true")); has {
		t.Error("$all annot pointer present in rawDB after failed block — Discard did not work")
	}
	if has, _ := s.rawDB.Has(headKey); has {
		t.Error("headKey written to rawDB after failed block — block should not have committed")
	}

	// Store's in-memory head is also unchanged.
	if s.headNumber != 0 {
		t.Errorf("headNumber = %d after failed block, want 0", s.headNumber)
	}
}

// TestCacheStore_NothingBeforeCommit verifies that applying an op to a CacheStore
// leaves the real database completely untouched — all writes land in the staging
// memorydb — and that a single Commit call flushes everything atomically.
func TestCacheStore_NothingBeforeCommit(t *testing.T) {
	raw := rawdb.NewMemoryDatabase()

	cs, err := newCacheStore(raw, ethtypes.EmptyRootHash, 1)
	if err != nil {
		t.Fatalf("newCacheStore: %v", err)
	}
	cs.blockHash = testHash1
	cs.parentHash = common.Hash{}

	initSystemAccount(cs.stateDB)

	op := makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100)
	if err := cs.ApplyOp(op); err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	// After ApplyOp but before Commit: real DB is completely clean.
	if has, _ := raw.Has(idKey(1)); has {
		t.Error("idKey(1) written to rawDB before Commit — staging isolation broken")
	}
	if has, _ := raw.Has(headKey); has {
		t.Error("headKey written to rawDB before Commit — staging isolation broken")
	}
	if has, _ := raw.Has(annotKey("$all", "true")); has {
		t.Error("$all annot pointer written to rawDB before Commit — staging isolation broken")
	}

	// Commit flushes everything in one atomic batch.
	if _, err := cs.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// After Commit: all entries are present in the real DB.
	if has, _ := raw.Has(idKey(1)); !has {
		t.Error("idKey(1) missing from rawDB after Commit")
	}
	if has, _ := raw.Has(headKey); !has {
		t.Error("headKey missing from rawDB after Commit")
	}
	if has, _ := raw.Has(annotKey("$all", "true")); !has {
		t.Error("$all annot pointer missing from rawDB after Commit")
	}
}

// TestReorg_ProducesCorrectFinalState verifies that Reorg atomically reverts
// old blocks and applies new blocks, leaving exactly the state defined by the
// new chain — with no residue from the reverted chain.
func TestReorg_ProducesCorrectFinalState(t *testing.T) {
	s := NewMemory()
	testHash3 := common.HexToHash("0xd3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3")
	testHash4 := common.HexToHash("0xd4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4")

	// Old chain: block 1 creates entity A, block 2 creates entity B.
	if _, err := s.ProcessBlock(makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "a", "text/plain", 100),
	)); err != nil {
		t.Fatalf("ProcessBlock 1: %v", err)
	}
	if _, err := s.ProcessBlock(makeBlock(2, testHash2, testHash1,
		makeCreate(testKey2, testSender, testOwner1, "b", "text/plain", 200),
	)); err != nil {
		t.Fatalf("ProcessBlock 2: %v", err)
	}

	// Reorg: revert both old blocks (newest first), then apply new chain.
	// New block 1 is empty; new block 2 creates entity C at testKey2/testAddr2.
	_, err := s.Reorg(
		[]types.ArkivBlockRef{
			{Number: hexutil.Uint64(2), Hash: testHash2},
			{Number: hexutil.Uint64(1), Hash: testHash1},
		},
		[]types.ArkivBlock{
			makeBlock(1, testHash3, common.Hash{}),
			makeBlock(2, testHash4, testHash3,
				makeCreate(testKey2, testSender, testOwner2, "c", "text/plain", 300),
			),
		},
	)
	if err != nil {
		t.Fatalf("Reorg: %v", err)
	}

	// Head is at new block 2.
	if s.headNumber != 2 {
		t.Errorf("headNumber = %d, want 2", s.headNumber)
	}
	if s.headHash != testHash4 {
		t.Errorf("headHash = %s, want %s", s.headHash, testHash4)
	}

	// Entity A (old chain, testAddr1) is gone.
	if entityExists(t, s, testAddr1) {
		t.Error("entity A (testAddr1) still present after reorg — revert incomplete")
	}

	// Entity C (new chain, testAddr2) is present with its new attributes.
	if !entityExists(t, s, testAddr2) {
		t.Fatal("entity C (testAddr2) missing after reorg")
	}
	e := getEntity(t, s, testAddr2)
	if string(e.Payload) != "c" {
		t.Errorf("entity C payload = %q, want \"c\"", e.Payload)
	}
	if e.Owner != testOwner2 {
		t.Errorf("entity C owner = %s, want %s", e.Owner, testOwner2)
	}

	// $all bitmap has exactly one entry (entity C, ID 1 after reorg).
	allBm := readBitmap(t, s, "$all", "true")
	if allBm.GetCardinality() != 1 {
		t.Errorf("$all bitmap cardinality = %d, want 1", allBm.GetCardinality())
	}
	if !allBm.Contains(1) {
		t.Error("$all bitmap missing entity C (ID 1)")
	}
}

// TestCacheStore_DeleteUpdatesAnnotPointerNotDeletes verifies the "no PebbleDB
// deletes in the forward path" invariant for the Delete operation.
//
// When the last entity in an annotation bucket is deleted, mutateBitmap writes
// an empty-bitmap value via db.Put() — it does NOT call db.Delete() on the
// annot pointer key. This matters because StagingDB.commit() uses NewIterator,
// which skips staging keys that have been deleted; any db.Delete() call on the
// forward path would be silently swallowed and the real DB would be left stale.
//
// After a Delete op commits: the annot key still exists in the real DB (pointing
// to an empty bitmap), not absent.
func TestCacheStore_DeleteUpdatesAnnotPointerNotDeletes(t *testing.T) {
	s := NewMemory()

	// Block 1: create the only entity (ID 1).
	if _, err := s.ProcessBlock(makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100),
	)); err != nil {
		t.Fatalf("ProcessBlock create: %v", err)
	}

	// Sanity: $all bitmap contains ID 1.
	if bm := readBitmap(t, s, "$all", "true"); !bm.Contains(1) {
		t.Fatal("$all bitmap missing entity ID 1 before delete")
	}

	// Block 2: delete the entity — leaves $all bitmap empty.
	if _, err := s.ProcessBlock(makeBlock(2, testHash2, testHash1,
		types.ArkivOperation{Delete: &types.DeleteOp{EntityKey: testKey1}},
	)); err != nil {
		t.Fatalf("ProcessBlock delete: %v", err)
	}

	// The annot pointer key must still exist in the real DB (updated via Put, not Delete).
	if has, _ := s.rawDB.Has(annotKey("$all", "true")); !has {
		t.Fatal("$all annot pointer was deleted from rawDB — forward path issued a db.Delete(), breaking staging commit()")
	}

	// The pointer must now reference an empty bitmap (not the old non-empty one).
	bm := readBitmap(t, s, "$all", "true")
	if bm.Contains(1) {
		t.Error("$all bitmap still contains entity ID 1 after delete")
	}
	if bm.GetCardinality() != 0 {
		t.Errorf("$all bitmap cardinality = %d after deleting only entity, want 0", bm.GetCardinality())
	}
}

// TestProcessBlock_EmptyBlockAdvancesHead verifies that a block with no
// operations still commits successfully: the canonical head pointer advances
// and all block index entries are written to the real DB.
func TestProcessBlock_EmptyBlockAdvancesHead(t *testing.T) {
	s := NewMemory()

	root, err := s.ProcessBlock(makeBlock(1, testHash1, common.Hash{}))
	if err != nil {
		t.Fatalf("ProcessBlock empty block: %v", err)
	}

	// In-memory head advances.
	if s.headNumber != 1 {
		t.Errorf("headNumber = %d, want 1", s.headNumber)
	}
	if s.headHash != testHash1 {
		t.Errorf("headHash = %s, want %s", s.headHash, testHash1)
	}
	if root == (common.Hash{}) {
		t.Error("ProcessBlock returned zero root for empty block")
	}

	// arkiv_head is persisted to rawDB with correct values.
	b, err := s.rawDB.Get(headKey)
	if err != nil {
		t.Fatalf("headKey not found in rawDB: %v", err)
	}
	gotNumber, gotHash, gotRoot := decodeHead(b)
	if gotNumber != 1 {
		t.Errorf("persisted headNumber = %d, want 1", gotNumber)
	}
	if gotHash != testHash1 {
		t.Errorf("persisted headHash = %s, want %s", gotHash, testHash1)
	}
	if gotRoot != root {
		t.Errorf("persisted headRoot = %s, want %s", gotRoot, root)
	}

	// Block index entries (rootKey, parentKey, blockNumberKey) are written.
	if has, _ := s.rawDB.Has(rootKey(testHash1)); !has {
		t.Error("rootKey missing for empty block")
	}
	if has, _ := s.rawDB.Has(parentKey(testHash1)); !has {
		t.Error("parentKey missing for empty block")
	}
	if has, _ := s.rawDB.Has(blockNumberKey(1)); !has {
		t.Error("blockNumberKey missing for empty block")
	}
}
