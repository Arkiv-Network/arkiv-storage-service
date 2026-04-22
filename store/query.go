package store

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
)

// HeadHash returns the current canonical head block hash.
func (s *Store) HeadHash() common.Hash {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headHash
}

// HeadNumber returns the current canonical head block number.
func (s *Store) HeadNumber() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.headNumber
}

// openStateAt opens a read-only StateDB at the given block number's state root.
// atBlockNumber == 0 means the current canonical head.
func (s *Store) openStateAt(atBlockNumber uint64) (*state.StateDB, error) {
	var root common.Hash
	if atBlockNumber == 0 {
		s.mu.RLock()
		root = s.headRoot
		s.mu.RUnlock()
	} else {
		hashBytes, err := s.rawDB.Get(blockNumberKey(atBlockNumber))
		if err != nil {
			return nil, fmt.Errorf("block %d not found: %w", atBlockNumber, err)
		}
		rootBytes, err := s.rawDB.Get(rootKey(common.BytesToHash(hashBytes)))
		if err != nil {
			return nil, fmt.Errorf("state root not found for block %d: %w", atBlockNumber, err)
		}
		root = common.BytesToHash(rootBytes)
	}
	return state.New(root, s.stateDB)
}

// ReadAnnotBitmap reads the roaring64 bitmap for (key, val) as committed in
// the trie at the given block's state. atBlockNumber == 0 means the current
// canonical head. The bitmap hash is read from the system account trie slot
// and is historically provable via eth_getProof.
func (s *Store) ReadAnnotBitmap(atBlockNumber uint64, key, val string) (*roaring64.Bitmap, error) {
	sdb, err := s.openStateAt(atBlockNumber)
	if err != nil {
		return nil, err
	}
	h := sdb.GetState(systemAddress, annotSlotKey(key, val))
	if h == (common.Hash{}) {
		return roaring64.New(), nil
	}
	bmBytes, err := s.rawDB.Get(bitmapKey(h))
	if err != nil || len(bmBytes) == 0 {
		return roaring64.New(), nil
	}
	bm := roaring64.New()
	if err := bm.UnmarshalBinary(bmBytes); err != nil {
		return nil, fmt.Errorf("unmarshal bitmap for (%s, %s): %w", key, val, err)
	}
	return bm, nil
}

// AllEntities returns the bitmap of all live entity IDs at the given block's state.
// atBlockNumber == 0 means the current canonical head.
func (s *Store) AllEntities(atBlockNumber uint64) (*roaring64.Bitmap, error) {
	return s.ReadAnnotBitmap(atBlockNumber, "$all", "true")
}

// IterateNumericAnnotBitmaps ORs together all bitmap entries for key whose
// numeric value (8-byte big-endian) falls in [lo, hi] (both inclusive).
// Scans the latest PebbleDB annotation pointer index, not a historical trie state.
func (s *Store) IterateNumericAnnotBitmaps(key string, lo, hi uint64) *roaring64.Bitmap {
	prefix := annotKeyPrefix(key)

	var startSuffix []byte
	if lo > 0 {
		startSuffix = make([]byte, 8)
		binary.BigEndian.PutUint64(startSuffix, lo)
	}

	result := roaring64.New()
	it := s.rawDB.NewIterator(prefix, startSuffix)
	defer it.Release()

	for it.Next() {
		k := it.Key()
		// Numeric entries have exactly 8 bytes of suffix.
		if len(k) != len(prefix)+8 {
			continue
		}
		numVal := binary.BigEndian.Uint64(k[len(prefix):])
		if numVal > hi {
			break
		}
		if bm := s.loadBitmapFromPointer(it.Value()); bm != nil {
			result.Or(bm)
		}
	}
	return result
}

// IterateStringAnnotBitmaps ORs together all bitmap entries for key whose
// string value falls within [lo, hi]. Bounds are optional (nil = unbounded);
// loIncl and hiIncl control inclusivity. Scans the latest PebbleDB pointer index.
func (s *Store) IterateStringAnnotBitmaps(key string, lo *string, loIncl bool, hi *string, hiIncl bool) *roaring64.Bitmap {
	prefix := annotKeyPrefix(key)

	var startSuffix []byte
	if lo != nil {
		startSuffix = []byte(*lo)
	}

	result := roaring64.New()
	it := s.rawDB.NewIterator(prefix, startSuffix)
	defer it.Release()

	for it.Next() {
		k := it.Key()
		if len(k) <= len(prefix) {
			continue
		}
		suffix := string(k[len(prefix):])

		if lo != nil && !loIncl && suffix == *lo {
			continue
		}
		if hi != nil && (suffix > *hi || (suffix == *hi && !hiIncl)) {
			break
		}
		if bm := s.loadBitmapFromPointer(it.Value()); bm != nil {
			result.Or(bm)
		}
	}
	return result
}

// IteratePrefixAnnotBitmaps ORs together all bitmap entries for key whose
// string value starts with prefix. Uses a range scan rather than a full scan.
func (s *Store) IteratePrefixAnnotBitmaps(key, prefix string) *roaring64.Bitmap {
	hi := prefixSuccessor(prefix)
	return s.IterateStringAnnotBitmaps(key, &prefix, true, hi, false)
}

// IterateGlobAnnotBitmaps ORs together all bitmap entries for key whose
// string value matches the glob pattern. Performs a full namespace scan.
func (s *Store) IterateGlobAnnotBitmaps(key, pattern string) *roaring64.Bitmap {
	prefix := annotKeyPrefix(key)

	result := roaring64.New()
	it := s.rawDB.NewIterator(prefix, nil)
	defer it.Release()

	for it.Next() {
		k := it.Key()
		if len(k) <= len(prefix) {
			continue
		}
		suffix := string(k[len(prefix):])
		if !globMatch(pattern, suffix) {
			continue
		}
		if bm := s.loadBitmapFromPointer(it.Value()); bm != nil {
			result.Or(bm)
		}
	}
	return result
}

// GetEntityRLP returns the decoded entity at addr using the state at atBlockNumber.
// atBlockNumber == 0 means the current canonical head.
func (s *Store) GetEntityRLP(atBlockNumber uint64, addr common.Address) (EntityRLP, error) {
	sdb, err := s.openStateAt(atBlockNumber)
	if err != nil {
		return EntityRLP{}, err
	}
	code := sdb.GetCode(addr)
	if len(code) == 0 {
		return EntityRLP{}, fmt.Errorf("entity not found: %s", addr)
	}
	return decodeEntity(code)
}

// GetEntityBytes returns the raw RLP-encoded entity bytes at addr at atBlockNumber.
// atBlockNumber == 0 means the current canonical head.
func (s *Store) GetEntityBytes(atBlockNumber uint64, addr common.Address) ([]byte, error) {
	sdb, err := s.openStateAt(atBlockNumber)
	if err != nil {
		return nil, err
	}
	code := sdb.GetCode(addr)
	if len(code) == 0 {
		return nil, fmt.Errorf("entity not found: %s", addr)
	}
	return code, nil
}

// IDToAddress returns the entity address for the given numeric ID.
// Returns the zero address and false if not found.
func (s *Store) IDToAddress(id uint64) (common.Address, bool) {
	data, err := s.rawDB.Get(idKey(id))
	if err != nil || len(data) != 20 {
		return common.Address{}, false
	}
	return common.BytesToAddress(data), true
}

// annotKeyPrefix returns the PebbleDB key prefix for all bitmap pointer entries
// under a given annotation key (i.e. "arkiv_annot" + key + "\x00").
func annotKeyPrefix(key string) []byte {
	k := append([]byte{}, prefixAnnot...)
	k = append(k, []byte(key)...)
	return append(k, 0x00)
}

// loadBitmapFromPointer loads the roaring64 bitmap stored at the content-addressed
// location pointed to by hashBytes (a 32-byte keccak256 hash). Returns nil on error.
func (s *Store) loadBitmapFromPointer(hashBytes []byte) *roaring64.Bitmap {
	if len(hashBytes) != common.HashLength {
		return nil
	}
	h := common.BytesToHash(hashBytes)
	bmBytes, err := s.rawDB.Get(bitmapKey(h))
	if err != nil || len(bmBytes) == 0 {
		return nil
	}
	bm := roaring64.New()
	if err := bm.UnmarshalBinary(bmBytes); err != nil {
		return nil
	}
	return bm
}

// prefixSuccessor returns the shortest string that is lexicographically greater
// than every string with the given prefix. Returns nil if the prefix is all 0xFF
// bytes (no successor exists within the byte range).
func prefixSuccessor(prefix string) *string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xFF {
			b[i]++
			s := string(b[:i+1])
			return &s
		}
	}
	return nil
}

// globMatch reports whether s matches the glob pattern.
// '*' matches any sequence of characters (including empty).
// '?' matches exactly one character.
// Matching is case-sensitive and byte-wise, consistent with SQLite GLOB.
func globMatch(pattern, s string) bool {
	for len(pattern) > 0 {
		if pattern[0] == '*' {
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if len(pattern) == 0 {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatch(pattern, s[i:]) {
					return true
				}
			}
			return false
		}
		if len(s) == 0 {
			return false
		}
		if pattern[0] == '?' || pattern[0] == s[0] {
			pattern = pattern[1:]
			s = s[1:]
		} else {
			return false
		}
	}
	return len(s) == 0
}

// isPrefixGlob reports whether pattern is a simple "prefix*" glob with no other
// wildcards. Returns the prefix if so.
func isPrefixGlob(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, "*") {
		return "", false
	}
	prefix := pattern[:len(pattern)-1]
	if strings.ContainsAny(prefix, "*?") {
		return "", false
	}
	return prefix, true
}
