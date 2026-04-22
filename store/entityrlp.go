package store

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// EntityRLP is the on-trie representation of an entity, stored as the code
// field of the entity account. keccak256(RLP(entity)) is the account's codeHash.
type EntityRLP struct {
	Payload            []byte
	Owner              common.Address
	Creator            common.Address
	ExpiresAt          uint64
	CreatedAtBlock     uint64
	ContentType        string
	// Key is the full 32-byte derivation key: keccak256(blockNumber || txSeq || opSeq).
	// The entity address is only the first 20 bytes of this hash; the last 12 bytes
	// are not recoverable from the address alone. Key is stored here so callers can
	// reconstruct the derivation inputs from an entity address without a separate index.
	//
	// OPEN QUESTION: if the query API never needs reverse-derivation (address →
	// blockNumber/txSeq/opSeq), Key can be dropped from EntityRLP. Removing it would
	// shrink every entity blob and also allow TxSeq and OpSeq to be dropped from
	// CreateOp in the ExEx→EntityDB wire format — a breaking API change requiring
	// coordination with the ExEx. See notes.md §4.
	Key                common.Hash
	StringAnnotations  []stringAnnotRLP
	NumericAnnotations []numericAnnotRLP
}

type stringAnnotRLP struct {
	Key   string
	Value string
}

type numericAnnotRLP struct {
	Key   string
	Value uint64
}

func encodeEntity(e EntityRLP) ([]byte, common.Hash, error) {
	data, err := rlp.EncodeToBytes(e)
	if err != nil {
		return nil, common.Hash{}, err
	}
	return data, crypto.Keccak256Hash(data), nil
}

func decodeEntity(data []byte) (EntityRLP, error) {
	var e EntityRLP
	return e, rlp.DecodeBytes(data, &e)
}

// DecodeEntityRLP decodes a raw RLP-encoded entity blob into an EntityRLP.
// Used by callers outside the store package (e.g. the query server).
func DecodeEntityRLP(data []byte) (EntityRLP, error) {
	return decodeEntity(data)
}

// deriveEntityKey computes keccak256(blockNumber || txSeq || opSeq).
func deriveEntityKey(blockNumber uint64, txSeq, opSeq uint32) common.Hash {
	var buf [16]byte
	encodeUint64BigEndian(buf[:8], blockNumber)
	encodeBigEndian32(buf[8:12], txSeq)
	encodeBigEndian32(buf[12:16], opSeq)
	return crypto.Keccak256Hash(buf[:])
}

func encodeUint64BigEndian(b []byte, v uint64) {
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
}

func encodeBigEndian32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}
