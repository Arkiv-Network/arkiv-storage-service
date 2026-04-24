package store

import (
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
)

// Entity is the on-trie representation of an entity, stored as the code
// field of the entity account. keccak256(RLP(entity)) is the account's codeHash.
type Entity struct {
	Payload            []byte
	Owner              common.Address
	Creator            common.Address
	ExpiresAt          uint64
	CreatedAtBlock     uint64
	ContentType        string
	// Key is the contract's entityKey: keccak256(chainId || registry || owner || nonce).
	// The trie account address is Key[:20]; the last 12 bytes are not recoverable from
	// the address alone. Key is stored here so the query API can return it to clients,
	// who need it to call EntityRegistry.commitment(entityKey) for payload verification.
	Key                common.Hash
	StringAnnotations  []stringAnnot
	NumericAnnotations []numericAnnot
}

type stringAnnot struct {
	Key   string
	Value string
}

type numericAnnot struct {
	Key   string
	Value uint64
}

func encodeEntity(e Entity) ([]byte, common.Hash, error) {
	data, err := rlp.EncodeToBytes(e)
	if err != nil {
		return nil, common.Hash{}, err
	}
	return data, crypto.Keccak256Hash(data), nil
}

func decodeEntity(data []byte) (Entity, error) {
	var e Entity
	return e, rlp.DecodeBytes(data, &e)
}

// DecodeEntity decodes a raw RLP-encoded entity blob.
// Used by callers outside the store package (e.g. the query server).
func DecodeEntity(data []byte) (Entity, error) {
	return decodeEntity(data)
}
