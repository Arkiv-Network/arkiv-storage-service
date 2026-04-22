package store

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/crypto"
)

// systemAddress is the address of the system account: keccak256("arkiv.system")[:20].
var systemAddress = common.BytesToAddress(crypto.Keccak256([]byte("arkiv.system")))

var entityCountSlot = crypto.Keccak256Hash([]byte("entity_count"))

// initSystemAccount creates the system account on first use.
// nonce=1 is required to prevent EIP-161 pruning (account has no code).
func initSystemAccount(sdb *state.StateDB) {
	if !sdb.Exist(systemAddress) {
		sdb.CreateAccount(systemAddress)
		sdb.SetNonce(systemAddress, 1, tracing.NonceChangeUnspecified)
	}
}

func readEntityCount(sdb *state.StateDB) uint64 {
	val := sdb.GetState(systemAddress, entityCountSlot)
	return binary.BigEndian.Uint64(val[24:]) // stored in the low 8 bytes of a 32-byte slot
}

// incrementEntityCount increments the entity counter and returns the new value (the assigned ID).
func incrementEntityCount(sdb *state.StateDB) uint64 {
	id := readEntityCount(sdb) + 1
	var slot common.Hash
	binary.BigEndian.PutUint64(slot[24:], id)
	sdb.SetState(systemAddress, entityCountSlot, slot)
	return id
}

// annotSlotKey returns the trie slot key for a bitmap hash: keccak256("annot" || annotKey || 0x00 || annotVal).
func annotSlotKey(key, val string) common.Hash {
	data := append([]byte("annot"), []byte(key)...)
	data = append(data, 0x00)
	data = append(data, []byte(val)...)
	return crypto.Keccak256Hash(data)
}

// idSlotKey returns the trie slot key for an ID→address mapping: keccak256("id" || uint64_id).
func idSlotKey(id uint64) common.Hash {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return crypto.Keccak256Hash(append([]byte("id"), b[:]...))
}

func setAnnotSlot(sdb *state.StateDB, key, val string, hash common.Hash) {
	sdb.SetState(systemAddress, annotSlotKey(key, val), hash)
}

func setIDSlot(sdb *state.StateDB, id uint64, addr common.Address) {
	var slot common.Hash
	copy(slot[12:], addr.Bytes()) // address in the low 20 bytes
	sdb.SetState(systemAddress, idSlotKey(id), slot)
}

func clearIDSlot(sdb *state.StateDB, id uint64) {
	sdb.SetState(systemAddress, idSlotKey(id), common.Hash{})
}
