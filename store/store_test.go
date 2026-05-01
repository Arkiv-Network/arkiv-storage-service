package store

import (
	"strings"
	"testing"

	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/RoaringBitmap/roaring/v2/roaring64"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

// Fixed addresses and hashes used across store tests.
var (
	// testKey1/2: 32-byte entity keys whose first 20 bytes match testAddr1/2.
	testKey1   = common.HexToHash("0x1111111111111111111111111111111111111111000000000000000000000000")
	testKey2   = common.HexToHash("0x2222222222222222222222222222222222222222000000000000000000000000")
	testAddr1  = common.Address(testKey1[:20])
	testAddr2  = common.Address(testKey2[:20])
	testSender = common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	testOwner1 = common.HexToAddress("0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	testOwner2 = common.HexToAddress("0xCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC")
	testHash1  = common.HexToHash("0xd1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1")
	testHash2  = common.HexToHash("0xd2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2")
)

// makeBlock builds an ArkivBlock from the given arguments.
// All ops are placed in a single transaction with testSender as the sender.
func makeBlock(number uint64, hash, parentHash common.Hash, ops ...types.ArkivOperation) types.ArkivBlock {
	var txs []types.ArkivTransaction
	if len(ops) > 0 {
		txs = []types.ArkivTransaction{{Sender: testSender, Operations: ops}}
	}
	return types.ArkivBlock{
		Header: types.ArkivBlockHeader{
			Number:     hexutil.Uint64(number),
			Hash:       hash,
			ParentHash: parentHash,
		},
		Transactions: txs,
	}
}

// makeCreate builds a simple create operation with no user annotations.
func makeCreate(entityKey common.Hash, sender, owner common.Address, payload, contentType string, expiresAt uint64) types.ArkivOperation {
	return types.ArkivOperation{
		Create: &types.CreateOp{
			EntityKey:   entityKey,
			Payload:     hexutil.Bytes(payload),
			ContentType: contentType,
			ExpiresAt:   hexutil.Uint64(expiresAt),
			Owner:       owner,
		},
	}
}

// openState opens a read-only StateDB at the store's current canonical head root.
func openState(t *testing.T, s *Store) *state.StateDB {
	t.Helper()
	sdb, err := state.New(s.headRoot, s.stateDB)
	if err != nil {
		t.Fatalf("open state at %s: %v", s.headRoot, err)
	}
	return sdb
}

// getEntity decodes the entity at addr from the current canonical state.
// Fails the test if the entity is absent.
func getEntity(t *testing.T, s *Store, addr common.Address) Entity {
	t.Helper()
	code := openState(t, s).GetCode(addr)
	if len(code) == 0 {
		t.Fatalf("entity %s not found in canonical state", addr)
	}
	e, err := decodeEntity(code)
	if err != nil {
		t.Fatalf("decodeEntity at %s: %v", addr, err)
	}
	return e
}

// entityExists reports whether addr has entity code in the current canonical state.
func entityExists(t *testing.T, s *Store, addr common.Address) bool {
	t.Helper()
	return len(openState(t, s).GetCode(addr)) > 0
}

// readBitmap returns the current roaring64 bitmap for (key, val).
// Returns an empty bitmap if no pointer has been written yet.
func readBitmap(t *testing.T, s *Store, key, val string) *roaring64.Bitmap {
	t.Helper()
	hashBytes, err := s.rawDB.Get(annotKey(key, val))
	if err != nil {
		return roaring64.New()
	}
	bmBytes, err := s.rawDB.Get(bitmapKey(common.BytesToHash(hashBytes)))
	if err != nil {
		t.Fatalf("bitmap bytes not found for (%s, %s): %v", key, val, err)
	}
	bm := roaring64.New()
	if err := bm.UnmarshalBinary(bmBytes); err != nil {
		t.Fatalf("unmarshal bitmap for (%s, %s): %v", key, val, err)
	}
	return bm
}

func TestCreateEntity(t *testing.T) {
	s := NewMemory()

	block := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, `{"msg":"hello"}`, "application/json", 1000),
	)
	if _, err := s.ProcessBlock(block); err != nil {
		t.Fatalf("ProcessBlock: %v", err)
	}

	// Head advances.
	if s.headNumber != 1 {
		t.Errorf("headNumber = %d, want 1", s.headNumber)
	}
	if s.headHash != testHash1 {
		t.Errorf("headHash = %s, want %s", s.headHash, testHash1)
	}

	// Entity RLP fields are correctly stored.
	e := getEntity(t, s, testAddr1)
	if string(e.Payload) != `{"msg":"hello"}` {
		t.Errorf("Payload = %q, want %q", e.Payload, `{"msg":"hello"}`)
	}
	if e.Owner != testOwner1 {
		t.Errorf("Owner = %s, want %s", e.Owner, testOwner1)
	}
	if e.Creator != testSender {
		t.Errorf("Creator = %s, want %s", e.Creator, testSender)
	}
	if e.ExpiresAt != 1000 {
		t.Errorf("ExpiresAt = %d, want 1000", e.ExpiresAt)
	}
	if e.CreatedAtBlock != 1 {
		t.Errorf("CreatedAtBlock = %d, want 1", e.CreatedAtBlock)
	}
	// On Create, LastModifiedAtBlock equals CreatedAtBlock; tx/op index reflect
	// the creating op's position (single-tx, single-op block → both 0).
	if e.LastModifiedAtBlock != 1 {
		t.Errorf("LastModifiedAtBlock = %d, want 1", e.LastModifiedAtBlock)
	}
	if e.TransactionIndexInBlock != 0 {
		t.Errorf("TransactionIndexInBlock = %d, want 0", e.TransactionIndexInBlock)
	}
	if e.OperationIndexInTransaction != 0 {
		t.Errorf("OperationIndexInTransaction = %d, want 0", e.OperationIndexInTransaction)
	}
	if e.ContentType != "application/json" {
		t.Errorf("ContentType = %q, want %q", e.ContentType, "application/json")
	}

	// Key is the contract's entityKey forwarded by the ExEx.
	if e.Key != testKey1 {
		t.Errorf("Key = %s, want %s", e.Key, testKey1)
	}

	// PebbleDB ID/addr mappings are written.
	idBytes, err := s.rawDB.Get(idKey(1))
	if err != nil {
		t.Fatalf("idKey(1) not found: %v", err)
	}
	if common.BytesToAddress(idBytes) != testAddr1 {
		t.Errorf("idKey(1) → %s, want %s", common.BytesToAddress(idBytes), testAddr1)
	}
	addrBytes, err := s.rawDB.Get(addrKey(testAddr1))
	if err != nil {
		t.Fatalf("addrKey not found: %v", err)
	}
	if decodeUint64(addrBytes) != 1 {
		t.Errorf("addrKey → %d, want 1", decodeUint64(addrBytes))
	}

	// Built-in bitmaps contain entity ID 1.
	if bm := readBitmap(t, s, "$all", "true"); !bm.Contains(1) {
		t.Error("$all bitmap does not contain entity ID 1")
	}
	if bm := readBitmap(t, s, "$owner", strings.ToLower(testOwner1.Hex())); !bm.Contains(1) {
		t.Error("$owner bitmap does not contain entity ID 1")
	}
	if bm := readBitmap(t, s, "$contentType", "application/json"); !bm.Contains(1) {
		t.Error("$contentType bitmap does not contain entity ID 1")
	}
	if bm := readBitmap(t, s, "$lastModifiedAtBlock", numericVal(1)); !bm.Contains(1) {
		t.Error("$lastModifiedAtBlock=1 bitmap does not contain entity ID 1")
	}
}

func TestUpdateEntity(t *testing.T) {
	s := NewMemory()

	block1 := makeBlock(1, testHash1, common.Hash{},
		types.ArkivOperation{Create: &types.CreateOp{
			EntityKey:   testKey1,
			Payload:     hexutil.Bytes("original"),
			ContentType: "text/plain",
			ExpiresAt:   hexutil.Uint64(500),
			Owner:       testOwner1,
			Attributes:  []types.Attribute{{ValueType: "string", Name: "type", Value: hexutil.Bytes("note")}},
		}},
	)
	if _, err := s.ProcessBlock(block1); err != nil {
		t.Fatalf("ProcessBlock 1: %v", err)
	}

	block2 := makeBlock(2, testHash2, testHash1,
		types.ArkivOperation{Update: &types.UpdateOp{
			EntityKey:   testKey1,
			Payload:     hexutil.Bytes("updated"),
			ContentType: "text/plain",
			Attributes:  []types.Attribute{{ValueType: "string", Name: "type", Value: hexutil.Bytes("doc")}},
		}},
	)
	if _, err := s.ProcessBlock(block2); err != nil {
		t.Fatalf("ProcessBlock 2: %v", err)
	}

	e := getEntity(t, s, testAddr1)
	if string(e.Payload) != "updated" {
		t.Errorf("Payload = %q, want %q", e.Payload, "updated")
	}
	// Update does not change expiration in v2; it stays at the value set on create.
	if e.ExpiresAt != 500 {
		t.Errorf("ExpiresAt = %d, want 500 (unchanged by Update)", e.ExpiresAt)
	}
	// Owner and Creator are immutable under Update.
	if e.Owner != testOwner1 {
		t.Errorf("Owner changed to %s", e.Owner)
	}
	if e.Creator != testSender {
		t.Errorf("Creator changed to %s", e.Creator)
	}

	// CreatedAtBlock is preserved; LastModifiedAtBlock is refreshed to the
	// update block; tx/op index identify the create event and are preserved.
	if e.CreatedAtBlock != 1 {
		t.Errorf("CreatedAtBlock = %d, want 1 (preserved)", e.CreatedAtBlock)
	}
	if e.LastModifiedAtBlock != 2 {
		t.Errorf("LastModifiedAtBlock = %d, want 2 (refreshed by Update)", e.LastModifiedAtBlock)
	}
	if e.TransactionIndexInBlock != 0 {
		t.Errorf("TransactionIndexInBlock = %d, want 0 (preserved from create)", e.TransactionIndexInBlock)
	}
	if e.OperationIndexInTransaction != 0 {
		t.Errorf("OperationIndexInTransaction = %d, want 0 (preserved from create)", e.OperationIndexInTransaction)
	}

	// Removed annotation value is no longer in the bitmap.
	if bm := readBitmap(t, s, "type", "note"); bm.Contains(1) {
		t.Error("old type=note bitmap still contains entity ID 1")
	}
	// New annotation value is in the bitmap.
	if bm := readBitmap(t, s, "type", "doc"); !bm.Contains(1) {
		t.Error("new type=doc bitmap does not contain entity ID 1")
	}
	// Unchanged annotation ($all) is still present.
	if bm := readBitmap(t, s, "$all", "true"); !bm.Contains(1) {
		t.Error("$all bitmap lost entity ID 1 after update")
	}
	// $lastModifiedAtBlock bitmap moves from value 1 (create block) to value 2
	// (update block).
	if bm := readBitmap(t, s, "$lastModifiedAtBlock", numericVal(1)); bm.Contains(1) {
		t.Error("$lastModifiedAtBlock=1 bitmap still contains entity ID 1 after update")
	}
	if bm := readBitmap(t, s, "$lastModifiedAtBlock", numericVal(2)); !bm.Contains(1) {
		t.Error("$lastModifiedAtBlock=2 bitmap does not contain entity ID 1 after update")
	}
}

// TestCreateRecordsTxAndOpIndex verifies that processBlock injects the
// transaction-level Index into CreateOp, and that the per-op OpIndex is
// preserved through to the Entity. Uses a multi-tx block so tx.Index > 0
// actually exercises the injection path (a tx-0 test would pass even if the
// injection were silently dropped).
func TestCreateRecordsTxAndOpIndex(t *testing.T) {
	s := NewMemory()

	createA := types.ArkivOperation{Create: &types.CreateOp{
		OpIndex:     5,
		EntityKey:   testKey1,
		Owner:       testOwner1,
		Payload:     hexutil.Bytes("a"),
		ContentType: "text/plain",
		ExpiresAt:   hexutil.Uint64(100),
	}}
	createB := types.ArkivOperation{Create: &types.CreateOp{
		OpIndex:     7,
		EntityKey:   testKey2,
		Owner:       testOwner2,
		Payload:     hexutil.Bytes("b"),
		ContentType: "text/plain",
		ExpiresAt:   hexutil.Uint64(200),
	}}

	block := types.ArkivBlock{
		Header: types.ArkivBlockHeader{
			Number:     hexutil.Uint64(1),
			Hash:       testHash1,
			ParentHash: common.Hash{},
		},
		Transactions: []types.ArkivTransaction{
			{Index: 0, Sender: testSender, Operations: []types.ArkivOperation{createA}},
			{Index: 3, Sender: testSender, Operations: []types.ArkivOperation{createB}},
		},
	}
	if _, err := s.ProcessBlock(block); err != nil {
		t.Fatalf("ProcessBlock: %v", err)
	}

	eA := getEntity(t, s, testAddr1)
	if eA.TransactionIndexInBlock != 0 {
		t.Errorf("entity A TransactionIndexInBlock = %d, want 0", eA.TransactionIndexInBlock)
	}
	if eA.OperationIndexInTransaction != 5 {
		t.Errorf("entity A OperationIndexInTransaction = %d, want 5", eA.OperationIndexInTransaction)
	}

	eB := getEntity(t, s, testAddr2)
	if eB.TransactionIndexInBlock != 3 {
		t.Errorf("entity B TransactionIndexInBlock = %d, want 3 (from tx.Index)", eB.TransactionIndexInBlock)
	}
	if eB.OperationIndexInTransaction != 7 {
		t.Errorf("entity B OperationIndexInTransaction = %d, want 7", eB.OperationIndexInTransaction)
	}
}

func TestDeleteEntity(t *testing.T) {
	s := NewMemory()

	block1 := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "payload", "text/plain", 999),
	)
	if _, err := s.ProcessBlock(block1); err != nil {
		t.Fatalf("ProcessBlock create: %v", err)
	}

	block2 := makeBlock(2, testHash2, testHash1,
		types.ArkivOperation{Delete: &types.DeleteOp{EntityKey: testKey1}},
	)
	if _, err := s.ProcessBlock(block2); err != nil {
		t.Fatalf("ProcessBlock delete: %v", err)
	}

	// Entity account is gone from the trie (pruned as EIP-161-empty).
	if entityExists(t, s, testAddr1) {
		t.Error("entity still exists in trie after delete")
	}

	// Bitmaps no longer contain entity ID 1.
	if bm := readBitmap(t, s, "$all", "true"); bm.Contains(1) {
		t.Error("$all bitmap still contains entity ID 1 after delete")
	}

	// arkiv_addr and arkiv_id entries are left as tombstones (not journaled).
	if has, _ := s.rawDB.Has(addrKey(testAddr1)); !has {
		t.Error("arkiv_addr tombstone missing after delete")
	}
	if has, _ := s.rawDB.Has(idKey(1)); !has {
		t.Error("arkiv_id tombstone missing after delete")
	}
}

func TestExtend(t *testing.T) {
	s := NewMemory()

	block1 := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100),
	)
	if _, err := s.ProcessBlock(block1); err != nil {
		t.Fatalf("ProcessBlock create: %v", err)
	}

	block2 := makeBlock(2, testHash2, testHash1,
		types.ArkivOperation{Extend: &types.ExtendOp{EntityKey: testKey1, ExpiresAt: hexutil.Uint64(200)}},
	)
	if _, err := s.ProcessBlock(block2); err != nil {
		t.Fatalf("ProcessBlock extend: %v", err)
	}

	e := getEntity(t, s, testAddr1)
	if e.ExpiresAt != 200 {
		t.Errorf("ExpiresAt = %d, want 200", e.ExpiresAt)
	}

	// Old $expiration bucket no longer contains entity.
	if bm := readBitmap(t, s, "$expiration", numericVal(100)); bm.Contains(1) {
		t.Error("old $expiration=100 bitmap still contains entity ID 1")
	}
	// New $expiration bucket contains entity.
	if bm := readBitmap(t, s, "$expiration", numericVal(200)); !bm.Contains(1) {
		t.Error("new $expiration=200 bitmap does not contain entity ID 1")
	}
}

func TestChangeOwner(t *testing.T) {
	s := NewMemory()

	block1 := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100),
	)
	if _, err := s.ProcessBlock(block1); err != nil {
		t.Fatalf("ProcessBlock create: %v", err)
	}

	block2 := makeBlock(2, testHash2, testHash1,
		types.ArkivOperation{Transfer: &types.TransferOp{EntityKey: testKey1, Owner: testOwner2}},
	)
	if _, err := s.ProcessBlock(block2); err != nil {
		t.Fatalf("ProcessBlock changeOwner: %v", err)
	}

	e := getEntity(t, s, testAddr1)
	if e.Owner != testOwner2 {
		t.Errorf("Owner = %s, want %s", e.Owner, testOwner2)
	}

	// Old $owner bucket no longer contains entity.
	if bm := readBitmap(t, s, "$owner", strings.ToLower(testOwner1.Hex())); bm.Contains(1) {
		t.Error("old $owner bitmap still contains entity ID 1")
	}
	// New $owner bucket contains entity.
	if bm := readBitmap(t, s, "$owner", strings.ToLower(testOwner2.Hex())); !bm.Contains(1) {
		t.Error("new $owner bitmap does not contain entity ID 1")
	}
}

func TestHeadPersistence(t *testing.T) {
	db := rawdb.NewMemoryDatabase()
	s1 := New(db)

	block := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100),
	)
	root, err := s1.ProcessBlock(block)
	if err != nil {
		t.Fatalf("ProcessBlock: %v", err)
	}

	// Construct a fresh Store from the same underlying database.
	s2 := New(db)
	if s2.headNumber != 1 {
		t.Errorf("headNumber = %d, want 1", s2.headNumber)
	}
	if s2.headHash != testHash1 {
		t.Errorf("headHash = %s, want %s", s2.headHash, testHash1)
	}
	if s2.headRoot != root {
		t.Errorf("headRoot = %s, want %s", s2.headRoot, root)
	}
}

func TestRevertBlock(t *testing.T) {
	s := NewMemory()

	block1 := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "p", "text/plain", 100),
	)
	if _, err := s.ProcessBlock(block1); err != nil {
		t.Fatalf("ProcessBlock: %v", err)
	}

	if _, err := s.RevertBlock(types.ArkivBlockRef{Number: hexutil.Uint64(1), Hash: testHash1}); err != nil {
		t.Fatalf("RevertBlock: %v", err)
	}

	// Head is restored to genesis.
	if s.headNumber != 0 {
		t.Errorf("headNumber = %d, want 0", s.headNumber)
	}
	if s.headRoot != ethtypes.EmptyRootHash {
		t.Errorf("headRoot = %s, want EmptyRootHash", s.headRoot)
	}

	// Entity is gone from the trie.
	if entityExists(t, s, testAddr1) {
		t.Error("entity still exists after revert")
	}

	// PebbleDB ID/addr mappings are removed (create was undone; no tombstone needed).
	if has, _ := s.rawDB.Has(idKey(1)); has {
		t.Error("idKey(1) still present after revert")
	}
	if has, _ := s.rawDB.Has(addrKey(testAddr1)); has {
		t.Error("addrKey still present after revert")
	}

	// Annotation bitmap pointer is reverted (key deleted).
	if has, _ := s.rawDB.Has(annotKey("$all", "true")); has {
		t.Error("$all annot pointer still present after revert")
	}
}

func TestReorgTwoBlocks(t *testing.T) {
	s := NewMemory()

	// Block 1: create entity A (ID 1).
	block1 := makeBlock(1, testHash1, common.Hash{},
		makeCreate(testKey1, testSender, testOwner1, "a", "text/plain", 100),
	)
	if _, err := s.ProcessBlock(block1); err != nil {
		t.Fatalf("ProcessBlock 1: %v", err)
	}

	// Block 2: create entity B (ID 2).
	block2 := makeBlock(2, testHash2, testHash1,
		makeCreate(testKey2, testSender, testOwner1, "b", "text/plain", 200),
	)
	if _, err := s.ProcessBlock(block2); err != nil {
		t.Fatalf("ProcessBlock 2: %v", err)
	}

	// Revert block 2: B is gone, A is still there.
	if _, err := s.RevertBlock(types.ArkivBlockRef{Number: hexutil.Uint64(2), Hash: testHash2}); err != nil {
		t.Fatalf("RevertBlock 2: %v", err)
	}
	if s.headNumber != 1 || s.headHash != testHash1 {
		t.Errorf("head = (%d, %s), want (1, %s)", s.headNumber, s.headHash, testHash1)
	}
	if !entityExists(t, s, testAddr1) {
		t.Error("entity A gone after reverting block 2")
	}
	if entityExists(t, s, testAddr2) {
		t.Error("entity B still present after reverting block 2")
	}
	bm := readBitmap(t, s, "$all", "true")
	if !bm.Contains(1) {
		t.Error("$all bitmap lost entity A (ID 1) after reverting block 2")
	}
	if bm.Contains(2) {
		t.Error("$all bitmap still has entity B (ID 2) after reverting block 2")
	}

	// Revert block 1: A is also gone.
	if _, err := s.RevertBlock(types.ArkivBlockRef{Number: hexutil.Uint64(1), Hash: testHash1}); err != nil {
		t.Fatalf("RevertBlock 1: %v", err)
	}
	if s.headNumber != 0 {
		t.Errorf("headNumber = %d, want 0", s.headNumber)
	}
	if entityExists(t, s, testAddr1) {
		t.Error("entity A still present after reverting block 1")
	}
	if bm := readBitmap(t, s, "$all", "true"); bm.GetCardinality() != 0 {
		t.Errorf("$all bitmap has %d entries, want 0", bm.GetCardinality())
	}
}
