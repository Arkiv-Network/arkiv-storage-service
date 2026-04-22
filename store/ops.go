package store

import (
	"fmt"
	"strings"

	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/ethdb"
)

// annotPair is a (key, val) annotation pair as stored in the bitmap index.
type annotPair struct {
	key string
	val string
}

// builtinAnnotations returns the built-in annotation pairs for an entity.
// Address and hash values are stored as lowercase hex so that the query
// normaliser (which lowercases $owner/$creator/$key query values) can match them.
func builtinAnnotations(e EntityRLP) []annotPair {
	return []annotPair{
		{"$all", "true"},
		{"$creator", strings.ToLower(e.Creator.Hex())},
		{"$createdAtBlock", numericVal(e.CreatedAtBlock)},
		{"$owner", strings.ToLower(e.Owner.Hex())},
		{"$key", strings.ToLower(e.Key.Hex())},
		{"$expiration", numericVal(e.ExpiresAt)},
		{"$contentType", e.ContentType},
	}
}

// userAnnotations returns the user-supplied annotation pairs for an entity.
func userAnnotations(e EntityRLP) []annotPair {
	var pairs []annotPair
	for _, a := range e.StringAnnotations {
		pairs = append(pairs, annotPair{a.Key, a.Value})
	}
	for _, a := range e.NumericAnnotations {
		pairs = append(pairs, annotPair{a.Key, numericVal(a.Value)})
	}
	return pairs
}

// allAnnotations returns built-ins + user annotations.
func allAnnotations(e EntityRLP) []annotPair {
	return append(builtinAnnotations(e), userAnnotations(e)...)
}

// annotPairSet builds a set of annotPairs for fast diffing.
func annotPairSet(pairs []annotPair) map[annotPair]struct{} {
	m := make(map[annotPair]struct{}, len(pairs))
	for _, p := range pairs {
		m[p] = struct{}{}
	}
	return m
}

// processCreate applies a Create operation.
func processCreate(db ethdb.Database, sdb *state.StateDB, j *blockJournal, op *types.CreateOp, blockNumber uint64) error {
	// 1. Assign entity ID.
	entityID := incrementEntityCount(sdb)

	// 2. Derive entity key.
	entityKey := deriveEntityKey(blockNumber, op.TxSeq, op.OpSeq)

	// 3. Create account in the trie.
	sdb.CreateAccount(op.EntityAddress)

	// 4. Write ID→address trie slot (trie-committed; reverts automatically on reorg).
	setIDSlot(sdb, entityID, op.EntityAddress)

	// 5. Write PebbleDB ID/addr mappings and journal them (mutable; explicit revert).
	idK := idKey(entityID)
	addrK := addrKey(op.EntityAddress)
	oldID, _ := db.Get(idK)
	oldAddr, _ := db.Get(addrK)
	j.record(idK, oldID)
	j.record(addrK, oldAddr)
	if err := db.Put(idK, op.EntityAddress.Bytes()); err != nil {
		return err
	}
	if err := db.Put(addrK, encodeUint64(entityID)); err != nil {
		return err
	}

	// 6. Build EntityRLP.
	entity := EntityRLP{
		Payload:        []byte(op.Payload),
		Owner:          op.Owner,
		Creator:        op.Sender,
		ExpiresAt:      op.ExpiresAt,
		CreatedAtBlock: blockNumber,
		ContentType:    op.ContentType,
		Key:            entityKey,
	}
	for _, a := range op.Annotations {
		if a.StringValue != nil {
			entity.StringAnnotations = append(entity.StringAnnotations, stringAnnotRLP{a.Key, *a.StringValue})
		} else if a.NumericValue != nil {
			entity.NumericAnnotations = append(entity.NumericAnnotations, numericAnnotRLP{a.Key, *a.NumericValue})
		}
	}

	// 7. Write annotation bitmaps.
	for _, pair := range allAnnotations(entity) {
		if err := bitmapAdd(db, sdb, j, pair.key, pair.val, entityID); err != nil {
			return fmt.Errorf("bitmap add %q: %w", pair.key, err)
		}
	}

	// 8. Encode entity and set as account code.
	data, _, err := encodeEntity(entity)
	if err != nil {
		return err
	}
	sdb.SetCode(op.EntityAddress, data, tracing.CodeChangeUnspecified)
	return nil
}

// processUpdate applies an Update operation.
func processUpdate(db ethdb.Database, sdb *state.StateDB, j *blockJournal, op *types.UpdateOp) error {
	code := sdb.GetCode(op.EntityAddress)
	if len(code) == 0 {
		return fmt.Errorf("entity %s not found", op.EntityAddress)
	}
	old, err := decodeEntity(code)
	if err != nil {
		return fmt.Errorf("decode entity: %w", err)
	}

	// Resolve entity ID for bitmap operations.
	addrK := addrKey(op.EntityAddress)
	idBytes, err := db.Get(addrK)
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	// Build new entity.
	updated := EntityRLP{
		Payload:        []byte(op.Payload),
		Owner:          old.Owner, // Update does not change owner
		Creator:        old.Creator,
		ExpiresAt:      op.ExpiresAt,
		CreatedAtBlock: old.CreatedAtBlock,
		ContentType:    op.ContentType,
		Key:            old.Key,
	}
	for _, a := range op.Annotations {
		if a.StringValue != nil {
			updated.StringAnnotations = append(updated.StringAnnotations, stringAnnotRLP{a.Key, *a.StringValue})
		} else if a.NumericValue != nil {
			updated.NumericAnnotations = append(updated.NumericAnnotations, numericAnnotRLP{a.Key, *a.NumericValue})
		}
	}

	// Diff annotation sets and update bitmaps.
	oldSet := annotPairSet(allAnnotations(old))
	newSet := annotPairSet(allAnnotations(updated))

	for pair := range oldSet {
		if _, kept := newSet[pair]; !kept {
			if err := bitmapRemove(db, sdb, j, pair.key, pair.val, entityID); err != nil {
				return fmt.Errorf("bitmap remove %q: %w", pair.key, err)
			}
		}
	}
	for pair := range newSet {
		if _, existed := oldSet[pair]; !existed {
			if err := bitmapAdd(db, sdb, j, pair.key, pair.val, entityID); err != nil {
				return fmt.Errorf("bitmap add %q: %w", pair.key, err)
			}
		}
	}

	data, _, err := encodeEntity(updated)
	if err != nil {
		return err
	}
	sdb.SetCode(op.EntityAddress, data, tracing.CodeChangeUnspecified)
	return nil
}

// processDelete applies a Delete operation.
func processDelete(db ethdb.Database, sdb *state.StateDB, j *blockJournal, op *types.DeleteOp) error {
	return deleteEntity(db, sdb, j, op.EntityAddress)
}

func deleteEntity(db ethdb.Database, sdb *state.StateDB, j *blockJournal, addr common.Address) error {
	code := sdb.GetCode(addr)
	if len(code) == 0 {
		return fmt.Errorf("entity %s not found", addr)
	}
	entity, err := decodeEntity(code)
	if err != nil {
		return fmt.Errorf("decode entity: %w", err)
	}

	// Resolve entity ID.
	idBytes, err := db.Get(addrKey(addr))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	// Remove from all annotation bitmaps.
	for _, pair := range allAnnotations(entity) {
		if err := bitmapRemove(db, sdb, j, pair.key, pair.val, entityID); err != nil {
			return fmt.Errorf("bitmap remove %q: %w", pair.key, err)
		}
	}

	// Clear trie-committed ID slot (reverts automatically on reorg).
	clearIDSlot(sdb, entityID)

	// SetCode(nil) → codeHash = emptyCodeHash → EIP-161-empty → pruned by Finalise.
	// arkiv_id and arkiv_addr are left as tombstones and NOT journaled.
	sdb.SetCode(addr, nil, tracing.CodeChangeUnspecified)
	return nil
}

// processExtend applies an Extend operation.
func processExtend(db ethdb.Database, sdb *state.StateDB, j *blockJournal, op *types.ExtendOp) error {
	code := sdb.GetCode(op.EntityAddress)
	if len(code) == 0 {
		return fmt.Errorf("entity %s not found", op.EntityAddress)
	}
	entity, err := decodeEntity(code)
	if err != nil {
		return fmt.Errorf("decode entity: %w", err)
	}

	idBytes, err := db.Get(addrKey(op.EntityAddress))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	// Update $expiration bitmap: remove old value, add new value.
	if err := bitmapRemove(db, sdb, j, "$expiration", numericVal(entity.ExpiresAt), entityID); err != nil {
		return err
	}
	entity.ExpiresAt = op.NewExpiresAt
	if err := bitmapAdd(db, sdb, j, "$expiration", numericVal(entity.ExpiresAt), entityID); err != nil {
		return err
	}

	data, _, err := encodeEntity(entity)
	if err != nil {
		return err
	}
	sdb.SetCode(op.EntityAddress, data, tracing.CodeChangeUnspecified)
	return nil
}

// processChangeOwner applies a ChangeOwner operation.
func processChangeOwner(db ethdb.Database, sdb *state.StateDB, j *blockJournal, op *types.ChangeOwnerOp) error {
	code := sdb.GetCode(op.EntityAddress)
	if len(code) == 0 {
		return fmt.Errorf("entity %s not found", op.EntityAddress)
	}
	entity, err := decodeEntity(code)
	if err != nil {
		return fmt.Errorf("decode entity: %w", err)
	}

	idBytes, err := db.Get(addrKey(op.EntityAddress))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	// Update $owner bitmap: remove old owner, add new owner.
	if err := bitmapRemove(db, sdb, j, "$owner", strings.ToLower(entity.Owner.Hex()), entityID); err != nil {
		return err
	}
	entity.Owner = op.NewOwner
	if err := bitmapAdd(db, sdb, j, "$owner", strings.ToLower(entity.Owner.Hex()), entityID); err != nil {
		return err
	}

	data, _, err := encodeEntity(entity)
	if err != nil {
		return err
	}
	sdb.SetCode(op.EntityAddress, data, tracing.CodeChangeUnspecified)
	return nil
}
