package store

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
)

// newTestStateDB returns a fresh StateDB backed by an in-memory database,
// with the system account already initialised.
func newTestStateDB(t *testing.T) *state.StateDB {
	t.Helper()
	s := NewMemory()
	sdb, err := state.New(s.headRoot, s.stateDB)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	initSystemAccount(sdb)
	return sdb
}

// TestIncrementEntityCount verifies that IDs are assigned sequentially
// starting from 1 and that readEntityCount reflects the latest value.
func TestIncrementEntityCount(t *testing.T) {
	sdb := newTestStateDB(t)

	id1 := incrementEntityCount(sdb)
	id2 := incrementEntityCount(sdb)
	id3 := incrementEntityCount(sdb)

	if id1 != 1 || id2 != 2 || id3 != 3 {
		t.Errorf("ids = %d, %d, %d; want 1, 2, 3", id1, id2, id3)
	}
	if got := readEntityCount(sdb); got != 3 {
		t.Errorf("readEntityCount = %d, want 3", got)
	}
}

// TestIDSlotRoundTrip verifies that setIDSlot packs the address into the low
// 20 bytes of the slot and that clearIDSlot zeroes it.
func TestIDSlotRoundTrip(t *testing.T) {
	sdb := newTestStateDB(t)

	addr := common.HexToAddress("0x1234567890AbcdEF1234567890AbCdef12345678")
	setIDSlot(sdb, 7, addr)

	slot := sdb.GetState(systemAddress, idSlotKey(7))
	// Address occupies the low 20 bytes (slot[12:32]).
	recovered := common.BytesToAddress(slot[12:])
	if recovered != addr {
		t.Errorf("recovered address = %s, want %s", recovered, addr)
	}
	// High 12 bytes must be zero.
	for i := 0; i < 12; i++ {
		if slot[i] != 0 {
			t.Errorf("slot[%d] = %02x, want 0x00", i, slot[i])
		}
	}

	clearIDSlot(sdb, 7)
	if slot := sdb.GetState(systemAddress, idSlotKey(7)); slot != (common.Hash{}) {
		t.Errorf("slot not zeroed after clearIDSlot: %s", slot)
	}
}

// TestInitSystemAccountIdempotent verifies that calling initSystemAccount
// twice does not reset the nonce or alter the account.
func TestInitSystemAccountIdempotent(t *testing.T) {
	s := NewMemory()
	sdb, err := state.New(ethtypes.EmptyRootHash, s.stateDB)
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}

	initSystemAccount(sdb)
	initSystemAccount(sdb) // second call must be a no-op

	nonce := sdb.GetNonce(systemAddress)
	if nonce != 1 {
		t.Errorf("nonce = %d, want 1", nonce)
	}
}
