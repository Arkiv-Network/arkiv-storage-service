package store

import (
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// TestBuiltinAnnotations verifies that all seven built-in annotation pairs
// are produced with the correct keys and encoded values.
func TestBuiltinAnnotations(t *testing.T) {
	creator := common.HexToAddress("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	owner := common.HexToAddress("0xBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB")
	key := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")

	e := EntityRLP{
		Owner:          owner,
		Creator:        creator,
		ExpiresAt:      1000,
		CreatedAtBlock: 42,
		ContentType:    "image/png",
		Key:            key,
	}

	pairs := builtinAnnotations(e)

	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		m[p.key] = p.val
	}

	want := map[string]string{
		"$all":            "true",
		"$creator":        strings.ToLower(creator.Hex()),
		"$owner":          strings.ToLower(owner.Hex()),
		"$key":            strings.ToLower(key.Hex()),
		"$contentType":    "image/png",
		"$createdAtBlock": numericVal(42),
		"$expiration":     numericVal(1000),
	}
	for k, v := range want {
		if got, ok := m[k]; !ok || got != v {
			t.Errorf("builtinAnnotations[%q] = %q (present=%v), want %q", k, got, ok, v)
		}
	}
	if len(pairs) != len(want) {
		t.Errorf("got %d built-in annotations, want %d", len(pairs), len(want))
	}
}

// TestAnnotPairSetDiff verifies that the set-difference logic used in
// processUpdate correctly identifies added and removed annotation pairs.
func TestAnnotPairSetDiff(t *testing.T) {
	pA := annotPair{"k", "a"}
	pB := annotPair{"k", "b"}
	pC := annotPair{"k", "c"}
	pD := annotPair{"k", "d"}

	// old = {A, B, C}, new = {B, C, D} → removed = {A}, added = {D}
	oldSet := annotPairSet([]annotPair{pA, pB, pC})
	newSet := annotPairSet([]annotPair{pB, pC, pD})

	var removed, added []annotPair
	for p := range oldSet {
		if _, kept := newSet[p]; !kept {
			removed = append(removed, p)
		}
	}
	for p := range newSet {
		if _, existed := oldSet[p]; !existed {
			added = append(added, p)
		}
	}

	if len(removed) != 1 || removed[0] != pA {
		t.Errorf("removed = %v, want [%v]", removed, pA)
	}
	if len(added) != 1 || added[0] != pD {
		t.Errorf("added = %v, want [%v]", added, pD)
	}
}
