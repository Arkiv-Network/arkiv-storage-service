package store

import (
	"encoding/binary"

	"github.com/ethereum/go-ethereum/common"
)

// PebbleDB key prefixes. All mutable entries are journaled for reorg handling;
// immutable entries (bm, pairs, root, parent) are never journaled or deleted.
var (
	prefixBitmap      = []byte("arkiv_bm")
	prefixAnnot       = []byte("arkiv_annot")
	prefixID          = []byte("arkiv_id")
	prefixAddr        = []byte("arkiv_addr")
	prefixPairs       = []byte("arkiv_pairs")
	prefixRoot        = []byte("arkiv_root")
	prefixParent      = []byte("arkiv_parent")
	prefixJournal     = []byte("arkiv_journal")
	prefixBlockNumber = []byte("arkiv_blknum")

	// headKey is a single fixed key storing the canonical head (number, hash, stateRoot).
	headKey = []byte("arkiv_head")
)

func bitmapKey(hash common.Hash) []byte {
	return append(append([]byte{}, prefixBitmap...), hash.Bytes()...)
}

func annotKey(key, val string) []byte {
	k := append([]byte{}, prefixAnnot...)
	k = append(k, []byte(key)...)
	k = append(k, 0x00)
	return append(k, []byte(val)...)
}

func idKey(id uint64) []byte {
	k := append([]byte{}, prefixID...)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], id)
	return append(k, b[:]...)
}

func addrKey(addr common.Address) []byte {
	return append(append([]byte{}, prefixAddr...), addr.Bytes()...)
}

func pairsKey(key, val string) []byte {
	k := append([]byte{}, prefixPairs...)
	k = append(k, []byte(key)...)
	k = append(k, 0x00)
	return append(k, []byte(val)...)
}

func rootKey(blockHash common.Hash) []byte {
	return append(append([]byte{}, prefixRoot...), blockHash.Bytes()...)
}

func parentKey(blockHash common.Hash) []byte {
	return append(append([]byte{}, prefixParent...), blockHash.Bytes()...)
}

func journalKey(blockNumber uint64, blockHash common.Hash, entryIndex uint32) []byte {
	k := append([]byte{}, prefixJournal...)
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], blockNumber)
	k = append(k, nb[:]...)
	k = append(k, blockHash.Bytes()...)
	var ib [4]byte
	binary.BigEndian.PutUint32(ib[:], entryIndex)
	return append(k, ib[:]...)
}

func blockNumberKey(number uint64) []byte {
	k := append([]byte{}, prefixBlockNumber...)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], number)
	return append(k, b[:]...)
}

func journalPrefix(blockNumber uint64, blockHash common.Hash) []byte {
	k := append([]byte{}, prefixJournal...)
	var nb [8]byte
	binary.BigEndian.PutUint64(nb[:], blockNumber)
	k = append(k, nb[:]...)
	return append(k, blockHash.Bytes()...)
}

// numericVal encodes a uint64 as 8-byte big-endian so that lexicographic key
// order matches numeric order. Used as the annotation value for numeric fields.
func numericVal(v uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return string(b[:])
}

func encodeUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func decodeUint64(b []byte) uint64 {
	return binary.BigEndian.Uint64(b)
}

// encodeHead serialises (blockNumber, blockHash, stateRoot) into 72 bytes.
func encodeHead(number uint64, hash, root common.Hash) []byte {
	b := make([]byte, 8+32+32)
	binary.BigEndian.PutUint64(b[:8], number)
	copy(b[8:40], hash.Bytes())
	copy(b[40:72], root.Bytes())
	return b
}

// decodeHead deserialises the 72-byte head record.
func decodeHead(b []byte) (number uint64, hash, root common.Hash) {
	number = binary.BigEndian.Uint64(b[:8])
	hash = common.BytesToHash(b[8:40])
	root = common.BytesToHash(b[40:72])
	return
}
