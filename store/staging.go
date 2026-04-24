package store

import (
	"fmt"

	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/ethdb/memorydb"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ethereum/go-ethereum/triedb/hashdb"
)

// StagingDB wraps an ethdb.Database and routes all writes to an in-memory
// memorydb.Database. Reads check the memorydb before the real DB. Nothing
// touches the real DB until commit() is called.
//
// NewBatch() delegates to the staging memorydb, so batch.Write() commits to
// the memorydb — not to the real DB. This is the mechanism by which
// trieDB.Commit()'s internal batch writes land in the staging layer: the TrieDB
// calls db.disk.NewBatch() → memorydb.batch.Write() → writes to memorydb.
type StagingDB struct {
	ethdb.Database              // real DB — read fallback + ancient storage
	staging *memorydb.Database  // in-memory staging store for this block
}

func newStagingDB(real ethdb.Database) *StagingDB {
	return &StagingDB{
		Database: real,
		staging:  memorydb.New(),
	}
}

func (s *StagingDB) Get(key []byte) ([]byte, error) {
	if v, err := s.staging.Get(key); err == nil {
		return v, nil
	}
	return s.Database.Get(key)
}

func (s *StagingDB) Has(key []byte) (bool, error) {
	if ok, _ := s.staging.Has(key); ok {
		return true, nil
	}
	return s.Database.Has(key)
}

func (s *StagingDB) Put(key, value []byte) error {
	return s.staging.Put(key, value)
}

func (s *StagingDB) Delete(key []byte) error {
	return s.staging.Delete(key)
}

// NewBatch returns a real memorydb batch. When the caller (e.g. trieDB.Commit)
// calls batch.Write(), writes land in s.staging — not on the real DB.
func (s *StagingDB) NewBatch() ethdb.Batch {
	return s.staging.NewBatch()
}

func (s *StagingDB) NewBatchWithSize(_ int) ethdb.Batch {
	return s.staging.NewBatch()
}

// commit copies all staged entries to the real DB in a single atomic batch.
// NewIterator snapshots live (non-deleted) keys only; this is correct because
// the ProcessBlock forward path never issues direct PebbleDB deletes.
func (s *StagingDB) commit() error {
	batch := s.Database.NewBatch()
	it := s.staging.NewIterator(nil, nil)
	defer it.Release()
	for it.Next() {
		if err := batch.Put(it.Key(), it.Value()); err != nil {
			return err
		}
	}
	return batch.Write()
}

func (s *StagingDB) discard() {
	s.staging = nil
}

// CacheStore is a write buffer for a single block. All trie mutations go through
// stateDB; all direct PebbleDB mutations go through stagingDB. Commit flushes
// both atomically via a single batch.Write(). Discard throws everything away.
type CacheStore struct {
	stagingDB   *StagingDB
	trieDB      *triedb.Database
	stateDB     *state.StateDB
	journal     *blockJournal
	blockNumber uint64
	blockHash   common.Hash
	parentHash  common.Hash
}

func newCacheStore(real ethdb.Database, parentRoot common.Hash, blockNumber uint64) (*CacheStore, error) {
	staging := newStagingDB(real)
	tdb := triedb.NewDatabase(staging, &triedb.Config{
		HashDB: &hashdb.Config{CleanCacheSize: 0}, // short-lived; no benefit caching
	})
	sdb := state.NewDatabase(tdb, nil)
	stateDB, err := state.New(parentRoot, sdb)
	if err != nil {
		staging.discard()
		return nil, fmt.Errorf("open state at %s: %w", parentRoot, err)
	}
	return &CacheStore{
		stagingDB:   staging,
		trieDB:      tdb,
		stateDB:     stateDB,
		journal:     &blockJournal{},
		blockNumber: blockNumber,
	}, nil
}

// ApplyOp applies a single Arkiv operation against the staged state.
func (c *CacheStore) ApplyOp(op types.ArkivOperation) error {
	return applyOp(c.stagingDB, c.stateDB, c.journal, op, c.blockNumber)
}

// Commit finalises the trie, flushes all staged writes atomically, and returns
// the new arkiv_stateRoot. Nothing touches the real DB until this call succeeds.
func (c *CacheStore) Commit() (common.Hash, error) {
	// 1. Finalise the StateDB; dirty trie nodes move into the per-block trieDB's
	//    memory cache.
	newRoot, err := c.stateDB.Commit(c.blockNumber, true, false)
	if err != nil {
		return common.Hash{}, fmt.Errorf("commit state: %w", err)
	}

	// 2. Flush the per-block trieDB into the staging memorydb.
	//    hashdb.Commit calls staging.NewBatch() → memorydb.batch → Write() →
	//    commits to staging memorydb. Not on disk yet.
	if err := c.trieDB.Commit(newRoot, false); err != nil {
		return common.Hash{}, fmt.Errorf("commit trie: %w", err)
	}

	// 3. Journal entries go into the staging memorydb.
	//    persist calls stagingDB.NewBatch() → memorydb.batch → Write() → memorydb.
	if err := c.journal.persist(c.stagingDB, c.blockNumber, c.blockHash); err != nil {
		return common.Hash{}, fmt.Errorf("persist journal: %w", err)
	}

	// 4. Block index entries.
	if err := c.stagingDB.Put(rootKey(c.blockHash), newRoot.Bytes()); err != nil {
		return common.Hash{}, err
	}
	if err := c.stagingDB.Put(parentKey(c.blockHash), c.parentHash.Bytes()); err != nil {
		return common.Hash{}, err
	}
	if err := c.stagingDB.Put(blockNumberKey(c.blockNumber), c.blockHash.Bytes()); err != nil {
		return common.Hash{}, err
	}

	// 5. Canonical head pointer — written last as the commit gate.
	//    A crash before this write leaves arkiv_head at the previous block;
	//    the store reopens in a consistent state. Orphaned trie nodes are
	//    harmless (HashScheme never deletes content-addressed entries by hash).
	if err := c.stagingDB.Put(headKey, encodeHead(c.blockNumber, c.blockHash, newRoot)); err != nil {
		return common.Hash{}, fmt.Errorf("stage head: %w", err)
	}

	// 6. Single atomic flush: trie nodes + entity blobs + bitmaps + ID maps +
	//    journal + block index + canonical head all land in one batch.Write().
	if err := c.stagingDB.commit(); err != nil {
		return common.Hash{}, fmt.Errorf("atomic flush: %w", err)
	}

	return newRoot, nil
}

// Discard throws away all staged writes. The real DB is not touched.
func (c *CacheStore) Discard() {
	c.stagingDB.discard()
}
