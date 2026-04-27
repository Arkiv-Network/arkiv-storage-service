package integration_test

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/Arkiv-Network/arkiv-storage-service/chain"
	"github.com/Arkiv-Network/arkiv-storage-service/query"
	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

var binaryPath string

func TestMain(m *testing.M) {
	tmp, err := os.CreateTemp("", "arkiv-storaged-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create temp file: %v\n", err)
		os.Exit(1)
	}
	tmp.Close()
	binaryPath = tmp.Name()

	build := exec.Command("go", "build", "-o", binaryPath, "../cmd/arkiv-storaged")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "build arkiv-storaged: %v\n", err)
		os.Remove(binaryPath)
		os.Exit(1)
	}

	code := m.Run()
	os.Remove(binaryPath)
	os.Exit(code)
}

// Entity keys whose first 20 bytes are the entity address.
var (
	iKey1 = common.HexToHash("0x1111111111111111111111111111111111111111000000000000000000000000")
	iKey2 = common.HexToHash("0x2222222222222222222222222222222222222222000000000000000000000000")
	iKey3 = common.HexToHash("0x3333333333333333333333333333333333333333000000000000000000000000")
	iKey4 = common.HexToHash("0x4444444444444444444444444444444444444444000000000000000000000000")

	iAddr1 = common.Address(iKey1[:20])
	iAddr2 = common.Address(iKey2[:20])
	iAddr3 = common.Address(iKey3[:20])


	iOwner1 = common.HexToAddress("0xaaaa000000000000000000000000000000000001")
	iOwner2 = common.HexToAddress("0xaaaa000000000000000000000000000000000002")
	iOwner3 = common.HexToAddress("0xaaaa000000000000000000000000000000000003")
	iSender = common.HexToAddress("0xdddddddddddddddddddddddddddddddddddddddd")
)

// bh returns a block hash for the given integer seed.
// bh(0) == common.Hash{} (genesis).
func bh(n uint64) common.Hash {
	var h common.Hash
	binary.BigEndian.PutUint64(h[24:], n)
	return h
}

// ----- op builders -----

func mkCreate(key common.Hash, owner common.Address, payload, ct string, expiresAt uint64, annots ...types.Annotation) types.ArkivOperation {
	return types.ArkivOperation{Create: &types.CreateOp{
		EntityKey:   key,
		Owner:       owner,
		Payload:     hexutil.Bytes(payload),
		ContentType: ct,
		ExpiresAt:   hexutil.Uint64(expiresAt),
		Annotations: annots,
	}}
}

func mkUpdate(key common.Hash, payload, ct string, expiresAt uint64, annots ...types.Annotation) types.ArkivOperation {
	return types.ArkivOperation{Update: &types.UpdateOp{
		EntityKey:   key,
		Payload:     hexutil.Bytes(payload),
		ContentType: ct,
		ExpiresAt:   hexutil.Uint64(expiresAt),
		Annotations: annots,
	}}
}

func mkDelete(key common.Hash) types.ArkivOperation {
	return types.ArkivOperation{Delete: &types.DeleteOp{EntityKey: key}}
}

func mkExtend(key common.Hash, newExpiresAt uint64) types.ArkivOperation {
	return types.ArkivOperation{Extend: &types.ExtendOp{EntityKey: key, ExpiresAt: hexutil.Uint64(newExpiresAt)}}
}

func mkChangeOwner(key common.Hash, newOwner common.Address) types.ArkivOperation {
	return types.ArkivOperation{ChangeOwner: &types.ChangeOwnerOp{EntityKey: key, NewOwner: newOwner}}
}

func mkBlock(num uint64, ops ...types.ArkivOperation) types.ArkivBlock {
	var txs []types.ArkivTransaction
	if len(ops) > 0 {
		txs = []types.ArkivTransaction{{Sender: iSender, Operations: ops}}
	}
	return types.ArkivBlock{
		Header: types.ArkivBlockHeader{
			Number:     hexutil.Uint64(num),
			Hash:       bh(num),
			ParentHash: bh(num - 1),
		},
		Transactions: txs,
	}
}

func mkBlockWithHash(num uint64, hash, parent common.Hash, ops ...types.ArkivOperation) types.ArkivBlock {
	var txs []types.ArkivTransaction
	if len(ops) > 0 {
		txs = []types.ArkivTransaction{{Sender: iSender, Operations: ops}}
	}
	return types.ArkivBlock{
		Header: types.ArkivBlockHeader{
			Number:     hexutil.Uint64(num),
			Hash:       hash,
			ParentHash: parent,
		},
		Transactions: txs,
	}
}

func mkBlockRef(num uint64) types.ArkivBlockRef {
	return types.ArkivBlockRef{Number: hexutil.Uint64(num), Hash: bh(num)}
}

func mkBlockRefWithHash(num uint64, hash common.Hash) types.ArkivBlockRef {
	return types.ArkivBlockRef{Number: hexutil.Uint64(num), Hash: hash}
}

func strAnnot(key, val string) types.Annotation {
	return types.Annotation{Key: key, StringValue: &val}
}

// ----- test environment -----

type testEnv struct {
	c *rpc.Client // chain client
	q *rpc.Client // query client
}

// freePort returns a free TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// waitReady blocks until the TCP address accepts connections, or the test fails.
func waitReady(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("server at %s did not become ready within 10s", addr)
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()

	chainPort := freePort(t)
	queryPort := freePort(t)

	cmd := exec.Command(binaryPath,
		"--chain-addr", fmt.Sprintf("127.0.0.1:%d", chainPort),
		"--query-addr", fmt.Sprintf("127.0.0.1:%d", queryPort),
		"--data-dir", t.TempDir(),
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start arkiv-storaged: %v", err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck
		cmd.Wait()                          //nolint:errcheck
	})

	chainAddr := fmt.Sprintf("127.0.0.1:%d", chainPort)
	queryAddr := fmt.Sprintf("127.0.0.1:%d", queryPort)
	waitReady(t, chainAddr)
	waitReady(t, queryAddr)

	chainClient, err := rpc.Dial("http://" + chainAddr)
	if err != nil {
		t.Fatalf("rpc.Dial chain: %v", err)
	}
	t.Cleanup(chainClient.Close)

	queryClient, err := rpc.Dial("http://" + queryAddr)
	if err != nil {
		t.Fatalf("rpc.Dial query: %v", err)
	}
	t.Cleanup(queryClient.Close)

	return &testEnv{c: chainClient, q: queryClient}
}

func (e *testEnv) commit(t *testing.T, blocks ...types.ArkivBlock) {
	t.Helper()
	if err := e.c.Call(nil, "arkiv_commitChain", chain.CommitChainRequest{Blocks: blocks}); err != nil {
		t.Fatalf("commitChain: %v", err)
	}
}

func (e *testEnv) revert(t *testing.T, refs ...types.ArkivBlockRef) {
	t.Helper()
	if err := e.c.Call(nil, "arkiv_revert", chain.RevertRequest{Blocks: refs}); err != nil {
		t.Fatalf("revert: %v", err)
	}
}

func (e *testEnv) reorg(t *testing.T, revertRefs []types.ArkivBlockRef, newBlocks []types.ArkivBlock) {
	t.Helper()
	if err := e.c.Call(nil, "arkiv_reorg", chain.ReorgRequest{
		RevertedBlocks: revertRefs,
		NewBlocks:      newBlocks,
	}); err != nil {
		t.Fatalf("reorg: %v", err)
	}
}

// doQuery evaluates a query string at the head (or a specific block via atBlock).
// opts may be nil (defaults to all fields, up to 200 results).
func (e *testEnv) doQuery(t *testing.T, queryStr string, opts *query.Options) *query.QueryResponse {
	t.Helper()
	var resp query.QueryResponse
	if err := e.q.Call(&resp, "arkiv_query", queryStr, opts); err != nil {
		t.Fatalf("query %q: %v", queryStr, err)
	}
	return &resp
}

func (e *testEnv) getEntity(t *testing.T, addr common.Address) *query.EntityData {
	t.Helper()
	var ed query.EntityData
	if err := e.q.Call(&ed, "arkiv_getEntityByAddress", addr, (*query.Options)(nil)); err != nil {
		t.Fatalf("getEntityByAddress %s: %v", addr, err)
	}
	return &ed
}

func (e *testEnv) assertEntityGone(t *testing.T, addr common.Address) {
	t.Helper()
	var ed query.EntityData
	if err := e.q.Call(&ed, "arkiv_getEntityByAddress", addr, (*query.Options)(nil)); err == nil {
		t.Errorf("expected entity %s to be gone, but found it", addr)
	}
}

func (e *testEnv) entityCount(t *testing.T) uint64 {
	t.Helper()
	var count uint64
	if err := e.q.Call(&count, "arkiv_getEntityCount"); err != nil {
		t.Fatalf("getEntityCount: %v", err)
	}
	return count
}

// ----- result helpers -----

func decodeEntity(t *testing.T, raw json.RawMessage) query.EntityData {
	t.Helper()
	var ed query.EntityData
	if err := json.Unmarshal(raw, &ed); err != nil {
		t.Fatalf("unmarshal EntityData: %v", err)
	}
	return ed
}

// queryKeys extracts and sorts the entity keys from a query response.
func queryKeys(t *testing.T, resp *query.QueryResponse) []common.Hash {
	t.Helper()
	var keys []common.Hash
	for _, raw := range resp.Data {
		ed := decodeEntity(t, raw)
		if ed.Key == nil {
			t.Fatal("EntityData has no Key field")
		}
		keys = append(keys, *ed.Key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Hex() < keys[j].Hex() })
	return keys
}

func assertKeys(t *testing.T, resp *query.QueryResponse, want ...common.Hash) {
	t.Helper()
	got := queryKeys(t, resp)
	wantSorted := make([]common.Hash, len(want))
	copy(wantSorted, want)
	sort.Slice(wantSorted, func(i, j int) bool { return wantSorted[i].Hex() < wantSorted[j].Hex() })
	if len(got) != len(wantSorted) {
		t.Errorf("query returned %d results, want %d: got keys %v, want %v", len(got), len(wantSorted), got, wantSorted)
		return
	}
	for i := range wantSorted {
		if got[i] != wantSorted[i] {
			t.Errorf("result[%d]: got key %s, want %s", i, got[i], wantSorted[i])
		}
	}
}

func assertEmpty(t *testing.T, resp *query.QueryResponse) {
	t.Helper()
	if len(resp.Data) != 0 {
		t.Errorf("expected empty result, got %d entities", len(resp.Data))
	}
}

// atBlock wraps a block number into Options.
func atBlock(n uint64) *query.Options {
	v := hexutil.Uint64(n)
	return &query.Options{AtBlock: &v}
}

// ----- tests -----

// TestEntityLifecycle exercises the full create→update→extend→changeOwner→delete
// lifecycle for two entities, verifying the query index at each step.
func TestEntityLifecycle(t *testing.T) {
	e := newTestEnv(t)

	// Block 1: create entity1 (owner1, text/plain, expires=1000, category=doc)
	// Block 2: create entity2 (owner2, text/html, expires=2000, category=img)
	e.commit(t,
		mkBlock(1, mkCreate(iKey1, iOwner1, "hello", "text/plain", 1000, strAnnot("category", "doc"))),
		mkBlock(2, mkCreate(iKey2, iOwner2, "world", "text/html", 2000, strAnnot("category", "img"))),
	)

	// Both entities visible.
	assertKeys(t, e.doQuery(t, "*", nil), iKey1, iKey2)
	if n := e.entityCount(t); n != 2 {
		t.Errorf("entityCount = %d, want 2", n)
	}

	// Owner filters.
	assertKeys(t, e.doQuery(t, "$owner = "+iOwner1.Hex(), nil), iKey1)
	assertKeys(t, e.doQuery(t, "$owner = "+iOwner2.Hex(), nil), iKey2)

	// Content type filter.
	assertKeys(t, e.doQuery(t, `$contentType = "text/html"`, nil), iKey2)

	// Expiration range.
	assertKeys(t, e.doQuery(t, "$expiration > 1500", nil), iKey2)
	assertKeys(t, e.doQuery(t, "$expiration <= 1000", nil), iKey1)

	// User annotation.
	assertKeys(t, e.doQuery(t, `category = "doc"`, nil), iKey1)
	assertKeys(t, e.doQuery(t, `category = "img"`, nil), iKey2)

	// GetEntityByAddress validates individual field values.
	ed := e.getEntity(t, iAddr1)
	if ed.Key == nil || *ed.Key != iKey1 {
		t.Errorf("Key = %v, want %s", ed.Key, iKey1)
	}
	if string(ed.Value) != "hello" {
		t.Errorf("Value = %q, want %q", string(ed.Value), "hello")
	}
	if ed.ContentType == nil || *ed.ContentType != "text/plain" {
		t.Errorf("ContentType = %v, want text/plain", ed.ContentType)
	}
	if ed.Owner == nil || *ed.Owner != iOwner1 {
		t.Errorf("Owner = %v, want %s", ed.Owner, iOwner1)
	}
	if ed.ExpiresAt == nil || *ed.ExpiresAt != 1000 {
		t.Errorf("ExpiresAt = %v, want 1000", ed.ExpiresAt)
	}
	if ed.CreatedAtBlock == nil || *ed.CreatedAtBlock != 1 {
		t.Errorf("CreatedAtBlock = %v, want 1", ed.CreatedAtBlock)
	}

	// Block 3: update entity1 — new payload, swap annotation, bump expiration.
	e.commit(t, mkBlock(3, mkUpdate(iKey1, "updated", "text/plain", 1500, strAnnot("category", "archive"))))

	if string(e.getEntity(t, iAddr1).Value) != "updated" {
		t.Error("entity1 payload not updated")
	}
	// Old annotation gone, new one present.
	assertEmpty(t, e.doQuery(t, `category = "doc"`, nil))
	assertKeys(t, e.doQuery(t, `category = "archive"`, nil), iKey1)
	// Both entities have expiration > 1400 now (entity1=1500, entity2=2000).
	assertKeys(t, e.doQuery(t, "$expiration > 1400", nil), iKey1, iKey2)

	// Block 4: extend entity1 to expiration 3000.
	e.commit(t, mkBlock(4, mkExtend(iKey1, 3000)))

	assertKeys(t, e.doQuery(t, "$expiration > 2500", nil), iKey1)

	// Block 5: change entity2's owner from owner2 to owner1.
	e.commit(t, mkBlock(5, mkChangeOwner(iKey2, iOwner1)))

	assertKeys(t, e.doQuery(t, "$owner = "+iOwner1.Hex(), nil), iKey1, iKey2)
	assertEmpty(t, e.doQuery(t, "$owner = "+iOwner2.Hex(), nil))

	// Block 6: delete entity1.
	e.commit(t, mkBlock(6, mkDelete(iKey1)))

	assertKeys(t, e.doQuery(t, "*", nil), iKey2)
	e.assertEntityGone(t, iAddr1)
	if n := e.entityCount(t); n != 1 {
		t.Errorf("entityCount = %d, want 1", n)
	}
}

// TestHistoricalQueries verifies that atBlock queries return the correct snapshot
// at each committed block height.
func TestHistoricalQueries(t *testing.T) {
	e := newTestEnv(t)

	// Block 1: create entity1.
	// Block 2: create entity2.
	// Block 3: delete entity1.
	e.commit(t,
		mkBlock(1, mkCreate(iKey1, iOwner1, "a", "text/plain", 100)),
		mkBlock(2, mkCreate(iKey2, iOwner2, "b", "text/plain", 200)),
		mkBlock(3, mkDelete(iKey1)),
	)

	// Historical snapshots.
	assertKeys(t, e.doQuery(t, "*", atBlock(1)), iKey1)
	assertKeys(t, e.doQuery(t, "*", atBlock(2)), iKey1, iKey2)
	assertKeys(t, e.doQuery(t, "*", atBlock(3)), iKey2)

	// Head (atBlock=0) == block 3.
	assertKeys(t, e.doQuery(t, "*", nil), iKey2)

	// Owner filter at historical block.
	assertKeys(t, e.doQuery(t, "$owner = "+iOwner1.Hex(), atBlock(2)), iKey1)
	assertEmpty(t, e.doQuery(t, "$owner = "+iOwner1.Hex(), atBlock(3)))

	// Block 4: update entity2 (change content type).
	e.commit(t, mkBlock(4, mkUpdate(iKey2, "b-updated", "text/html", 200)))

	assertKeys(t, e.doQuery(t, `$contentType = "text/plain"`, atBlock(2)), iKey1, iKey2)
	assertKeys(t, e.doQuery(t, `$contentType = "text/plain"`, atBlock(3)), iKey2)
	assertEmpty(t, e.doQuery(t, `$contentType = "text/plain"`, nil)) // entity2 is now text/html
	assertKeys(t, e.doQuery(t, `$contentType = "text/html"`, nil), iKey2)

	// Historical getEntityByAddress.
	edHistorical := e.getEntity(t, iAddr2) // head, should be updated
	if string(edHistorical.Value) != "b-updated" {
		t.Errorf("head entity2 payload = %q, want %q", string(edHistorical.Value), "b-updated")
	}
}

// TestRevertRestoresState verifies that reverting blocks brings the store back
// to its pre-commit state — both in the trie and in the annotation bitmaps.
func TestRevertRestoresState(t *testing.T) {
	e := newTestEnv(t)

	// Block 1: create entity1 with original payload.
	e.commit(t, mkBlock(1,
		mkCreate(iKey1, iOwner1, "original", "text/plain", 100, strAnnot("tag", "alpha")),
	))

	// Block 2: update entity1, create entity2.
	e.commit(t, mkBlock(2,
		mkUpdate(iKey1, "updated", "text/plain", 100, strAnnot("tag", "beta")),
		mkCreate(iKey2, iOwner2, "second", "text/html", 200),
	))

	// Sanity: both visible with updated state.
	assertKeys(t, e.doQuery(t, "*", nil), iKey1, iKey2)
	assertEmpty(t, e.doQuery(t, `tag = "alpha"`, nil))
	assertKeys(t, e.doQuery(t, `tag = "beta"`, nil), iKey1)
	if string(e.getEntity(t, iAddr1).Value) != "updated" {
		t.Fatal("setup: entity1 should show updated payload")
	}

	// Revert block 2.
	e.revert(t, mkBlockRef(2))

	// entity2 is gone; entity1 is back to its block-1 state.
	assertKeys(t, e.doQuery(t, "*", nil), iKey1)
	e.assertEntityGone(t, iAddr2)
	if string(e.getEntity(t, iAddr1).Value) != "original" {
		t.Error("entity1 payload not restored after revert")
	}
	// Annotation bitmaps are also restored.
	assertKeys(t, e.doQuery(t, `tag = "alpha"`, nil), iKey1)
	assertEmpty(t, e.doQuery(t, `tag = "beta"`, nil))

	// Revert block 1.
	e.revert(t, mkBlockRef(1))

	assertEmpty(t, e.doQuery(t, "*", nil))
	e.assertEntityGone(t, iAddr1)
	if n := e.entityCount(t); n != 0 {
		t.Errorf("entityCount = %d, want 0", n)
	}
}

// TestReorgSwapsBranch verifies that a reorg atomically reverts the old branch
// and applies the new one, leaving exactly the new branch's entities visible.
func TestReorgSwapsBranch(t *testing.T) {
	e := newTestEnv(t)

	// Original chain (hashes a1, a2):
	//   block 1: entity1 (owner1)
	//   block 2: entity2 (owner2)
	ha1 := common.HexToHash("0xaaaa000000000000000000000000000000000000000000000000000000000001")
	ha2 := common.HexToHash("0xaaaa000000000000000000000000000000000000000000000000000000000002")

	e.commit(t,
		mkBlockWithHash(1, ha1, bh(0), mkCreate(iKey1, iOwner1, "a", "text/plain", 100)),
		mkBlockWithHash(2, ha2, ha1, mkCreate(iKey2, iOwner2, "b", "text/plain", 200)),
	)

	assertKeys(t, e.doQuery(t, "*", nil), iKey1, iKey2)

	// Reorg chain (hashes b1, b2):
	//   block 1: entity3 (owner3)
	//   block 2: entity4 (owner1)
	hb1 := common.HexToHash("0xbbbb000000000000000000000000000000000000000000000000000000000001")
	hb2 := common.HexToHash("0xbbbb000000000000000000000000000000000000000000000000000000000002")

	e.reorg(t,
		[]types.ArkivBlockRef{
			mkBlockRefWithHash(2, ha2),
			mkBlockRefWithHash(1, ha1),
		},
		[]types.ArkivBlock{
			mkBlockWithHash(1, hb1, bh(0), mkCreate(iKey3, iOwner3, "c", "text/plain", 300)),
			mkBlockWithHash(2, hb2, hb1, mkCreate(iKey4, iOwner1, "d", "text/plain", 400)),
		},
	)

	// Old branch entities are gone.
	e.assertEntityGone(t, iAddr1)
	e.assertEntityGone(t, iAddr2)

	// New branch entities are present.
	assertKeys(t, e.doQuery(t, "*", nil), iKey3, iKey4)
	assertKeys(t, e.doQuery(t, "$owner = "+iOwner3.Hex(), nil), iKey3)
	assertKeys(t, e.doQuery(t, "$owner = "+iOwner1.Hex(), nil), iKey4)
	assertEmpty(t, e.doQuery(t, "$owner = "+iOwner2.Hex(), nil))

	if n := e.entityCount(t); n != 2 {
		t.Errorf("entityCount = %d, want 2", n)
	}

	// Entity fields on new branch are correct.
	ed3 := e.getEntity(t, iAddr3)
	if ed3.Key == nil || *ed3.Key != iKey3 {
		t.Errorf("entity3 Key = %v, want %s", ed3.Key, iKey3)
	}
	if string(ed3.Value) != "c" {
		t.Errorf("entity3 payload = %q, want %q", string(ed3.Value), "c")
	}
	if ed3.CreatedAtBlock == nil || *ed3.CreatedAtBlock != 1 {
		t.Errorf("entity3 CreatedAtBlock = %v, want 1", ed3.CreatedAtBlock)
	}
}

// TestMultipleReorgs verifies that sequential reorgs converge to the correct state.
func TestMultipleReorgs(t *testing.T) {
	e := newTestEnv(t)

	ha := common.HexToHash("0xaaaa000000000000000000000000000000000000000000000000000000000001")
	hb := common.HexToHash("0xbbbb000000000000000000000000000000000000000000000000000000000001")
	hc := common.HexToHash("0xcccc000000000000000000000000000000000000000000000000000000000001")

	// Original: block 1 with entity1.
	e.commit(t, mkBlockWithHash(1, ha, bh(0), mkCreate(iKey1, iOwner1, "a", "text/plain", 100)))
	assertKeys(t, e.doQuery(t, "*", nil), iKey1)

	// First reorg: replace block 1 with entity2.
	e.reorg(t,
		[]types.ArkivBlockRef{mkBlockRefWithHash(1, ha)},
		[]types.ArkivBlock{mkBlockWithHash(1, hb, bh(0), mkCreate(iKey2, iOwner2, "b", "text/plain", 200))},
	)
	e.assertEntityGone(t, iAddr1)
	assertKeys(t, e.doQuery(t, "*", nil), iKey2)

	// Second reorg: replace block 1 with entity3.
	e.reorg(t,
		[]types.ArkivBlockRef{mkBlockRefWithHash(1, hb)},
		[]types.ArkivBlock{mkBlockWithHash(1, hc, bh(0), mkCreate(iKey3, iOwner3, "c", "text/plain", 300))},
	)
	e.assertEntityGone(t, iAddr1)
	e.assertEntityGone(t, iAddr2)
	assertKeys(t, e.doQuery(t, "*", nil), iKey3)
	if n := e.entityCount(t); n != 1 {
		t.Errorf("entityCount = %d, want 1", n)
	}
}
