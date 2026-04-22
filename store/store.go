package store

import (
	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
)

// Store maintains the entity state index. It is the core of the storage service,
// backed by a Merkle Patricia Trie and PebbleDB annotation bitmaps.
type Store struct{}

func New() *Store {
	return &Store{}
}

// ProcessBlock applies all operations in the block and returns the new arkiv_stateRoot.
func (s *Store) ProcessBlock(block types.ArkivBlock) (common.Hash, error) {
	return common.Hash{}, nil
}

// RevertBlock undoes the effects of a previously processed block.
func (s *Store) RevertBlock(ref types.ArkivBlockRef) error {
	return nil
}
