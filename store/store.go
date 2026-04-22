package store

import (
	"fmt"
	"sync"

	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/state"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
)

// NewMemory creates a Store backed by an in-memory database, intended for
// testing and development.
func NewMemory() *Store {
	return New(rawdb.NewMemoryDatabase())
}

// Store maintains the entity state index. It wraps a Merkle Patricia Trie
// (go-ethereum StateDB with HashScheme) and PebbleDB annotation bitmaps.
//
// All historical state roots are retained by HashScheme, enabling point-in-time
// reads without a separate snapshot mechanism.
type Store struct {
	rawDB   ethdb.Database
	trieDB  *triedb.Database
	stateDB state.Database // wraps trieDB; re-used across blocks

	headRoot   common.Hash
	headHash   common.Hash
	headNumber uint64

	mu sync.Mutex
}

// New creates a Store backed by the provided database.
// If the database contains a previously persisted head, it is restored;
// otherwise the store starts from an empty state.
func New(raw ethdb.Database) *Store {
	tdb := triedb.NewDatabase(raw, &triedb.Config{
		HashDB: &hashdb.Config{CleanCacheSize: 64 * 1024 * 1024},
	})
	sdb := state.NewDatabase(tdb, nil)

	headNumber, headHash, headRoot := uint64(0), common.Hash{}, ethtypes.EmptyRootHash
	if b, err := raw.Get(headKey); err == nil && len(b) == 72 {
		headNumber, headHash, headRoot = decodeHead(b)
	}

	return &Store{
		rawDB:      raw,
		trieDB:     tdb,
		stateDB:    sdb,
		headRoot:   headRoot,
		headHash:   headHash,
		headNumber: headNumber,
	}
}

// ProcessBlock applies all operations in the block and returns the new arkiv_stateRoot.
func (s *Store) ProcessBlock(block types.ArkivBlock) (common.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Open a fresh StateDB at the current canonical state root.
	sdb, err := state.New(s.headRoot, s.stateDB)
	if err != nil {
		return common.Hash{}, fmt.Errorf("open state at %s: %w", s.headRoot, err)
	}

	// Ensure the system account exists (idempotent after first block).
	initSystemAccount(sdb)

	j := &blockJournal{}
	blockNumber := uint64(block.Header.Number)

	for i, op := range block.Operations {
		if err := applyOp(s.rawDB, sdb, j, op, blockNumber); err != nil {
			return common.Hash{}, fmt.Errorf("op %d: %w", i, err)
		}
	}

	// Commit trie changes and obtain new state root.
	newRoot, err := sdb.Commit(blockNumber, true, false)
	if err != nil {
		return common.Hash{}, fmt.Errorf("commit state: %w", err)
	}

	// Flush trie nodes to the underlying database.
	// false = do not garbage-collect old nodes; HashScheme retains all history.
	if err := s.trieDB.Commit(newRoot, false); err != nil {
		return common.Hash{}, fmt.Errorf("commit trie: %w", err)
	}

	// SAFETY: the trie and PebbleDB are separate stores with no shared transaction
	// boundary. A crash between TrieDB.Commit above and the writes below leaves the
	// two stores inconsistent: the trie has the new root but PebbleDB still reflects
	// the previous block. On restart the canonical head (arkiv_head) will not yet
	// point to the new root, so the store will re-open at the old head and the new
	// trie root will be orphaned. The mutable PebbleDB entries (bitmaps, ID maps)
	// will also be in the pre-block state, which is consistent with the old head.
	// The net effect is that the block is silently dropped — the ExEx will need to
	// re-deliver it. This is acceptable for now but should be hardened before
	// production: see notes.md §3.

	// Persist the per-block journal for mutable PebbleDB entries.
	if err := j.persist(s.rawDB, blockNumber, block.Header.Hash); err != nil {
		return common.Hash{}, fmt.Errorf("persist journal: %w", err)
	}

	// Record blockHash → stateRoot and blockHash → parentHash for revert.
	if err := s.rawDB.Put(rootKey(block.Header.Hash), newRoot.Bytes()); err != nil {
		return common.Hash{}, err
	}
	if err := s.rawDB.Put(parentKey(block.Header.Hash), block.Header.ParentHash.Bytes()); err != nil {
		return common.Hash{}, err
	}

	s.headRoot = newRoot
	s.headHash = block.Header.Hash
	s.headNumber = blockNumber

	if err := s.rawDB.Put(headKey, encodeHead(s.headNumber, s.headHash, s.headRoot)); err != nil {
		return common.Hash{}, fmt.Errorf("persist head: %w", err)
	}
	return newRoot, nil
}

// RevertBlock undoes the effects of a previously processed block and restores
// the canonical head to the block's parent.
func (s *Store) RevertBlock(ref types.ArkivBlockRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	blockNumber := uint64(ref.Number)

	// SAFETY: revert is also non-atomic across the journal replay and the head
	// write below. A crash after the journal batch commits but before arkiv_head
	// is updated leaves the mutable PebbleDB entries in the reverted state while
	// the canonical head still points to the (now-reverted) block. On restart the
	// store will open at the stale head and attempt to read a trie root that is
	// still valid (HashScheme retains it), but the PebbleDB bitmap/ID state will
	// be ahead of it. This inconsistency must be detected and resolved at startup.
	// See notes.md §3.

	// Replay the PebbleDB journal in reverse to undo mutable entries.
	// The trie reverts automatically via HashScheme (no trie writes needed).
	if err := revertBlockJournal(s.rawDB, blockNumber, ref.Hash); err != nil {
		return fmt.Errorf("revert journal: %w", err)
	}

	// Look up the parent hash to restore the canonical head.
	parentHashBytes, err := s.rawDB.Get(parentKey(ref.Hash))
	if err != nil {
		return fmt.Errorf("parent hash not found for block %s: %w", ref.Hash, err)
	}
	parentHash := common.BytesToHash(parentHashBytes)

	// Clean up the root and parent mappings for the reverted block.
	_ = s.rawDB.Delete(rootKey(ref.Hash))
	_ = s.rawDB.Delete(parentKey(ref.Hash))

	// Restore the parent's state root (may be EmptyRootHash for the genesis parent).
	var parentRoot common.Hash
	if rootBytes, err := s.rawDB.Get(rootKey(parentHash)); err == nil {
		parentRoot = common.BytesToHash(rootBytes)
	} else {
		parentRoot = ethtypes.EmptyRootHash
	}

	s.headHash = parentHash
	s.headRoot = parentRoot
	if blockNumber > 0 {
		s.headNumber = blockNumber - 1
	}

	if err := s.rawDB.Put(headKey, encodeHead(s.headNumber, s.headHash, s.headRoot)); err != nil {
		return fmt.Errorf("persist head: %w", err)
	}
	return nil
}

func applyOp(db ethdb.Database, sdb *state.StateDB, j *blockJournal, op types.ArkivOperation, blockNumber uint64) error {
	switch {
	case op.Create != nil:
		return processCreate(db, sdb, j, op.Create, blockNumber)
	case op.Update != nil:
		return processUpdate(db, sdb, j, op.Update)
	case op.Delete != nil:
		return processDelete(db, sdb, j, op.Delete)
	case op.Extend != nil:
		return processExtend(db, sdb, j, op.Extend)
	case op.ChangeOwner != nil:
		return processChangeOwner(db, sdb, j, op.ChangeOwner)
	default:
		return fmt.Errorf("empty operation")
	}
}
