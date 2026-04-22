package store

import (
	"testing"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// TestBitmapRoundTripStability checks that serialize → deserialize → re-serialize
// produces identical bytes. This is the property mutateBitmap actually depends on:
// we always read a stored bitmap via UnmarshalBinary before mutating it, so the
// hash of the re-serialized bytes must match the hash that was stored.
//
// The RoaringFormatSpec does NOT guarantee that two independently constructed
// bitmaps with identical contents serialize to the same bytes — a bitmap without
// run containers may use either SERIAL_COOKIE or SERIAL_COOKIE_NO_RUNCONTAINER.
// However, the spec notes that implementations can achieve isomorphism by always
// using SERIAL_COOKIE_NO_RUNCONTAINER for bitmaps without run containers.
//
// Tracing the roaring64 call chain (MarshalBinary → ToBytes → WriteTo →
// per-sub-bitmap roaringArray.writeTo) confirms that this library always uses
// SERIAL_COOKIE_NO_RUNCONTAINER unless RunOptimize() has been called. Since
// mutateBitmap never calls RunOptimize(), serialization is deterministic for our
// bitmaps and round-trip stability holds.
//
// See: https://github.com/RoaringBitmap/RoaringFormatSpec#isomorphism-to-its-binary-representation
func TestBitmapRoundTripStability(t *testing.T) {
	ids := []uint64{1, 5, 42, 1000, 999999}

	bm := roaring64.New()
	for _, id := range ids {
		bm.Add(id)
	}

	b1, err := bm.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary: %v", err)
	}

	// Deserialize into a fresh bitmap, then re-serialize.
	bm2 := roaring64.New()
	if err := bm2.UnmarshalBinary(b1); err != nil {
		t.Fatalf("UnmarshalBinary: %v", err)
	}
	b2, err := bm2.MarshalBinary()
	if err != nil {
		t.Fatalf("MarshalBinary (after round-trip): %v", err)
	}

	if string(b1) != string(b2) {
		t.Fatalf("round-trip is not stable: serialize→deserialize→serialize produced different bytes\nbefore: %x\nafter:  %x", b1, b2)
	}
}
