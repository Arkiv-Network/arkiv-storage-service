package query_test

import (
	"sort"
	"testing"

	"github.com/Arkiv-Network/arkiv-storage-service/query"
	"github.com/Arkiv-Network/arkiv-storage-service/store"
	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Fixed entity keys and addresses used in query evaluate tests.
// eKeyN[:20] == eAddrN, so the entity address is derived from the key.
var (
	eKey1 = common.HexToHash("0x1111111111111111111111111111111111111111000000000000000000000000")
	eKey2 = common.HexToHash("0x2222222222222222222222222222222222222222000000000000000000000000")
	eKey3 = common.HexToHash("0x3333333333333333333333333333333333333333000000000000000000000000")

	qOwner1 = common.HexToAddress("0xaaaa000000000000000000000000000000000001")
	qOwner2 = common.HexToAddress("0xaaaa000000000000000000000000000000000002")
	qSender = common.HexToAddress("0xdddddddddddddddddddddddddddddddddddddddd")

	qHash1 = common.HexToHash("0x1000000000000000000000000000000000000000000000000000000000000001")
	qHash2 = common.HexToHash("0x1000000000000000000000000000000000000000000000000000000000000002")
)

func makeQBlock(number uint64, hash, parent common.Hash, ops ...types.ArkivOperation) types.ArkivBlock {
	return types.ArkivBlock{
		Header: types.ArkivBlockHeader{
			Number:     hexutil.Uint64(number),
			Hash:       hash,
			ParentHash: parent,
		},
		Operations: ops,
	}
}

func makeQCreate(entityKey common.Hash, sender, owner common.Address, contentType string, expiresAt uint64, annots ...types.Annotation) types.ArkivOperation {
	return types.ArkivOperation{
		Create: &types.CreateOp{
			EntityKey:   entityKey,
			Sender:      sender,
			Payload:     hexutil.Bytes("payload"),
			ContentType: contentType,
			ExpiresAt:   expiresAt,
			Owner:       owner,
			Annotations: annots,
		},
	}
}

func mustProcess(t *testing.T, s *store.Store, block types.ArkivBlock) {
	t.Helper()
	if _, err := s.ProcessBlock(block); err != nil {
		t.Fatalf("ProcessBlock %d: %v", uint64(block.Header.Number), err)
	}
}

func evaluate(t *testing.T, s *store.Store, queryStr string) []uint64 {
	t.Helper()
	return evaluateAt(t, s, queryStr, 0)
}

func evaluateAt(t *testing.T, s *store.Store, queryStr string, atBlock uint64) []uint64 {
	t.Helper()
	ast, err := query.Parse(queryStr)
	if err != nil {
		t.Fatalf("Parse %q: %v", queryStr, err)
	}
	bm, err := ast.Evaluate(s, atBlock)
	if err != nil {
		t.Fatalf("Evaluate %q: %v", queryStr, err)
	}
	ids := bm.ToArray()
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func assertIDs(t *testing.T, got []uint64, want []uint64) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("got IDs %v, want %v", got, want)
		return
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got IDs %v, want %v", got, want)
			return
		}
	}
}

func strAnnot(key, val string) types.Annotation {
	return types.Annotation{Key: key, StringValue: &val}
}

func numAnnot(key string, val uint64) types.Annotation {
	return types.Annotation{Key: key, NumericValue: &val}
}

// TestEvaluateAll verifies that * and $all return every live entity.
func TestEvaluateAll(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner2, "text/plain", 200),
	))
	assertIDs(t, evaluate(t, s, "*"), []uint64{1, 2})
	assertIDs(t, evaluate(t, s, "$all"), []uint64{1, 2})
}

// TestEvaluateEquality verifies exact-match queries against built-in annotations.
func TestEvaluateEquality(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner2, "text/html", 200),
	))
	assertIDs(t, evaluate(t, s, `$owner = `+qOwner1.Hex()), []uint64{1})
	assertIDs(t, evaluate(t, s, `$owner = `+qOwner2.Hex()), []uint64{2})
	assertIDs(t, evaluate(t, s, `$contentType = "text/html"`), []uint64{2})
}

// TestEvaluateEqualityNot verifies != queries.
func TestEvaluateEqualityNot(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner2, "text/plain", 200),
		makeQCreate(eKey3, qSender, qOwner2, "text/plain", 300),
	))
	// entities 2 and 3 belong to owner2; owner1 != owner2 → only entity 1
	assertIDs(t, evaluate(t, s, `$owner != `+qOwner2.Hex()), []uint64{1})
}

// TestEvaluateAnd verifies AND queries.
func TestEvaluateAnd(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner2, "text/plain", 200),
		makeQCreate(eKey3, qSender, qOwner1, "text/html", 300),
	))
	// owner1 AND text/plain → only entity 1
	q := `$owner = ` + qOwner1.Hex() + ` && $contentType = "text/plain"`
	assertIDs(t, evaluate(t, s, q), []uint64{1})
}

// TestEvaluateOr verifies OR queries.
func TestEvaluateOr(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner2, "text/html", 200),
		makeQCreate(eKey3, qSender, qOwner2, "text/xml", 300),
	))
	q := `$owner = ` + qOwner1.Hex() + ` || $owner = ` + qOwner2.Hex()
	assertIDs(t, evaluate(t, s, q), []uint64{1, 2, 3})
}

// TestEvaluateNumericRange verifies range comparisons on $expiration.
func TestEvaluateNumericRange(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner1, "text/plain", 200),
		makeQCreate(eKey3, qSender, qOwner1, "text/plain", 300),
	))
	assertIDs(t, evaluate(t, s, `$expiration > 100`), []uint64{2, 3})
	assertIDs(t, evaluate(t, s, `$expiration < 300`), []uint64{1, 2})
	assertIDs(t, evaluate(t, s, `$expiration >= 200`), []uint64{2, 3})
	assertIDs(t, evaluate(t, s, `$expiration <= 200`), []uint64{1, 2})
}

// TestEvaluateNumericRangeEdge verifies boundary conditions for numeric range scans.
func TestEvaluateNumericRangeEdge(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 0),
	))
	assertIDs(t, evaluate(t, s, `$expiration < 0`), []uint64{})
	assertIDs(t, evaluate(t, s, `$expiration >= 0`), []uint64{1})
}

// TestEvaluateUserAnnotation verifies equality on user-defined string and numeric annotations.
func TestEvaluateUserAnnotation(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100, strAnnot("type", "document")),
		makeQCreate(eKey2, qSender, qOwner1, "text/plain", 200, strAnnot("type", "image")),
		makeQCreate(eKey3, qSender, qOwner1, "text/plain", 300, numAnnot("score", 42)),
	))
	assertIDs(t, evaluate(t, s, `type = "document"`), []uint64{1})
	assertIDs(t, evaluate(t, s, `type = "image"`), []uint64{2})
	assertIDs(t, evaluate(t, s, `score = 42`), []uint64{3})
}

// TestEvaluateInclusion verifies IN and NOT IN queries.
func TestEvaluateInclusion(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
		makeQCreate(eKey2, qSender, qOwner2, "text/plain", 200),
		makeQCreate(eKey3, qSender, qOwner2, "text/html", 300),
	))
	qIn := `$owner in (` + qOwner1.Hex() + ` ` + qOwner2.Hex() + `)`
	assertIDs(t, evaluate(t, s, qIn), []uint64{1, 2, 3})

	qNotIn := `$owner not in (` + qOwner2.Hex() + `)`
	assertIDs(t, evaluate(t, s, qNotIn), []uint64{1})
}

// TestEvaluateGlobPrefix verifies prefix glob queries on user-defined annotations.
func TestEvaluateGlobPrefix(t *testing.T) {
	s := store.NewMemory()
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100, strAnnot("mime", "text/plain")),
		makeQCreate(eKey2, qSender, qOwner1, "text/html", 200, strAnnot("mime", "text/html")),
		makeQCreate(eKey3, qSender, qOwner1, "image/png", 300, strAnnot("mime", "image/png")),
	))
	assertIDs(t, evaluate(t, s, `mime ~ "text/*"`), []uint64{1, 2})
	assertIDs(t, evaluate(t, s, `mime ~ "image/*"`), []uint64{3})
}

// TestEvaluateAtBlock verifies that historical queries return state at the requested block.
func TestEvaluateAtBlock(t *testing.T) {
	s := store.NewMemory()
	// Block 1: entity 1 only.
	mustProcess(t, s, makeQBlock(1, qHash1, common.Hash{},
		makeQCreate(eKey1, qSender, qOwner1, "text/plain", 100),
	))
	// Block 2: entity 2 added.
	mustProcess(t, s, makeQBlock(2, qHash2, qHash1,
		makeQCreate(eKey2, qSender, qOwner1, "text/plain", 200),
	))

	assertIDs(t, evaluateAt(t, s, "*", 1), []uint64{1})
	assertIDs(t, evaluateAt(t, s, "*", 2), []uint64{1, 2})
}
