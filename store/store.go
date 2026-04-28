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

// Store maintains the entity state index: a Merkle Patricia Trie (go-ethereum
// StateDB with HashScheme) and PebbleDB annotation bitmaps. Historical state
// roots are retained by HashScheme, enabling point-in-time reads.
//
// Writes use a per-block CacheStore (staging layer) that accumulates all
// mutations — trie nodes, entity blobs, bitmaps, ID maps, and the canonical
// head pointer — in an in-memory buffer and flushes them to PebbleDB in a
// single atomic batch.Write(). This guarantees that a crash mid-block never
// leaves the trie and bitmap index in inconsistent states.
//
// On reorg, the trie reverts for free via HashScheme. Mutable PebbleDB entries
// (arkiv_annot, arkiv_id, arkiv_addr) are repopulated from the system account
// trie slots at the reverted state root (see revert.go).
//
// trieDB and stateDB on this struct are used only for the read path (queries,
// historical state opens, and reorg repopulation). All forward writes go
// through per-block CacheStore instances.
type Store struct {
	rawDB   ethdb.Database
	trieDB  *triedb.Database
	stateDB state.Database

	headRoot   common.Hash
	headHash   common.Hash
	headNumber uint64

	mu sync.RWMutex
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
	return s.processBlock(block)
}

// HeadRoot returns the arkiv_stateRoot of the current canonical head.
func (s *Store) HeadRoot() common.Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headRoot
}

// RevertBlock undoes the effects of a previously processed block and restores
// the canonical head to the block's parent. Returns the new head state root.
func (s *Store) RevertBlock(ref types.ArkivBlockRef) (common.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.revertBlock(ref); err != nil {
		return common.Hash{}, err
	}
	return s.headRoot, nil
}

// RevertBlocks reverts a contiguous sequence of blocks (newest-first) in a
// single atomic operation, repopulating mutable PebbleDB entries once from the
// common ancestor. Returns the new head state root.
func (s *Store) RevertBlocks(refs []types.ArkivBlockRef) (common.Hash, error) {
	if len(refs) == 0 {
		s.mu.RLock()
		defer s.mu.RUnlock()
		return s.headRoot, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.revertToAncestor(refs); err != nil {
		return common.Hash{}, err
	}
	return s.headRoot, nil
}

// Reorg atomically reverts revertedBlocks (newest-first) then applies newBlocks
// (oldest-first). Returns the new head state root. The write lock is held for
// the full duration, so no query observes an intermediate state.
//
// Mutable PebbleDB entries are repopulated once from the common ancestor state
// root — not once per reverted block — so reorg cost is O(|arkiv_pairs| + |arkiv_id|)
// regardless of how many blocks are reverted.
func (s *Store) Reorg(revertedBlocks []types.ArkivBlockRef, newBlocks []types.ArkivBlock) (common.Hash, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(revertedBlocks) > 0 {
		if err := s.revertToAncestor(revertedBlocks); err != nil {
			return common.Hash{}, err
		}
	}
	for i, block := range newBlocks {
		if _, err := s.processBlock(block); err != nil {
			return common.Hash{}, fmt.Errorf("new block %d: %w", i, err)
		}
	}
	return s.headRoot, nil
}

// processBlock is the internal implementation of ProcessBlock. Callers must hold s.mu.
func (s *Store) processBlock(block types.ArkivBlock) (common.Hash, error) {
	blockNumber := uint64(block.Header.Number)

	cs, err := newCacheStore(s.rawDB, s.headRoot, blockNumber)
	if err != nil {
		return common.Hash{}, err
	}
	cs.blockHash = block.Header.Hash
	cs.parentHash = block.Header.ParentHash

	initSystemAccount(cs.stateDB)

	for _, tx := range block.Transactions {
		for i, op := range tx.Operations {
			// Sender lives at the transaction level on the wire; inject it
			// into CreateOp so processCreate can set the entity's Creator.
			if op.Create != nil {
				op.Create.Sender = tx.Sender
			}
			if err := cs.ApplyOp(op); err != nil {
				cs.Discard()
				return common.Hash{}, fmt.Errorf("tx %s op %d: %w", tx.Hash, i, err)
			}
		}
	}

	newRoot, err := cs.Commit()
	if err != nil {
		cs.Discard()
		return common.Hash{}, err
	}

	s.headRoot = newRoot
	s.headHash = block.Header.Hash
	s.headNumber = blockNumber
	return newRoot, nil
}

// revertBlock is the internal implementation of RevertBlock. Callers must hold s.mu.
// It reverts a single block: repopulates mutable PebbleDB entries from the trie
// at the parent state root, cleans up the block index, and updates the head
// pointer — all in a single atomic batch.Write().
func (s *Store) revertBlock(ref types.ArkivBlockRef) error {
	blockNumber := uint64(ref.Number)

	parentHashBytes, err := s.rawDB.Get(parentKey(ref.Hash))
	if err != nil {
		return fmt.Errorf("parent hash not found for block %s: %w", ref.Hash, err)
	}
	parentHash := common.BytesToHash(parentHashBytes)

	var parentRoot common.Hash
	if b, err := s.rawDB.Get(rootKey(parentHash)); err == nil {
		parentRoot = common.BytesToHash(b)
	} else {
		parentRoot = ethtypes.EmptyRootHash
	}

	sdb, err := state.New(parentRoot, s.stateDB)
	if err != nil {
		return fmt.Errorf("open state at %s: %w", parentRoot, err)
	}

	batch := s.rawDB.NewBatch()

	if err := repopulatePebbleDB(s.rawDB, batch, sdb); err != nil {
		return fmt.Errorf("repopulate pebble: %w", err)
	}
	if err := batch.Delete(rootKey(ref.Hash)); err != nil {
		return err
	}
	if err := batch.Delete(parentKey(ref.Hash)); err != nil {
		return err
	}
	if err := batch.Delete(blockNumberKey(blockNumber)); err != nil {
		return err
	}

	newHeadNumber := uint64(0)
	if blockNumber > 0 {
		newHeadNumber = blockNumber - 1
	}
	if err := batch.Put(headKey, encodeHead(newHeadNumber, parentHash, parentRoot)); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("atomic revert flush: %w", err)
	}

	s.headRoot = parentRoot
	s.headHash = parentHash
	s.headNumber = newHeadNumber
	return nil
}

// revertToAncestor reverts a sequence of blocks (newest-first) in a single
// atomic operation. Mutable PebbleDB entries are repopulated once from the
// common ancestor state root rather than once per reverted block.
// Callers must hold s.mu.
func (s *Store) revertToAncestor(revertedBlocks []types.ArkivBlockRef) error {
	// revertedBlocks is newest-first; the common ancestor is the parent of the oldest.
	oldest := revertedBlocks[len(revertedBlocks)-1]

	parentHashBytes, err := s.rawDB.Get(parentKey(oldest.Hash))
	if err != nil {
		return fmt.Errorf("parent hash not found for block %s: %w", oldest.Hash, err)
	}
	ancestorHash := common.BytesToHash(parentHashBytes)

	var ancestorRoot common.Hash
	if b, err := s.rawDB.Get(rootKey(ancestorHash)); err == nil {
		ancestorRoot = common.BytesToHash(b)
	} else {
		ancestorRoot = ethtypes.EmptyRootHash
	}

	sdb, err := state.New(ancestorRoot, s.stateDB)
	if err != nil {
		return fmt.Errorf("open state at %s: %w", ancestorRoot, err)
	}

	batch := s.rawDB.NewBatch()

	if err := repopulatePebbleDB(s.rawDB, batch, sdb); err != nil {
		return fmt.Errorf("repopulate pebble: %w", err)
	}
	for _, ref := range revertedBlocks {
		n := uint64(ref.Number)
		if err := batch.Delete(rootKey(ref.Hash)); err != nil {
			return err
		}
		if err := batch.Delete(parentKey(ref.Hash)); err != nil {
			return err
		}
		if err := batch.Delete(blockNumberKey(n)); err != nil {
			return err
		}
	}

	ancestorNumber := uint64(oldest.Number)
	if ancestorNumber > 0 {
		ancestorNumber--
	}
	if err := batch.Put(headKey, encodeHead(ancestorNumber, ancestorHash, ancestorRoot)); err != nil {
		return err
	}
	if err := batch.Write(); err != nil {
		return fmt.Errorf("atomic revert flush: %w", err)
	}

	s.headRoot = ancestorRoot
	s.headHash = ancestorHash
	s.headNumber = ancestorNumber
	return nil
}

