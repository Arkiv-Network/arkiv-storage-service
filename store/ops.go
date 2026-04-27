package store

import (
	"fmt"
	"strings"

	"github.com/Arkiv-Network/arkiv-storage-service/types"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/tracing"
)

// annotPair is a (key, val) annotation pair as stored in the bitmap index.
type annotPair struct {
	key string
	val string
}

// builtinAnnotations returns the built-in annotation pairs for an entity.
// Address and hash values are stored as lowercase hex so that the query
// normaliser (which lowercases $owner/$creator/$key query values) can match them.
func builtinAnnotations(e Entity) []annotPair {
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
func userAnnotations(e Entity) []annotPair {
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
func allAnnotations(e Entity) []annotPair {
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

// applyOp dispatches a single Arkiv operation to its handler.
func applyOp(cs *CacheStore, op types.ArkivOperation) error {
	switch {
	case op.Create != nil:
		return processCreate(cs, op.Create)
	case op.Update != nil:
		return processUpdate(cs, op.Update)
	case op.Delete != nil:
		return processDelete(cs, op.Delete)
	case op.Extend != nil:
		return processExtend(cs, op.Extend)
	case op.ChangeOwner != nil:
		return processChangeOwner(cs, op.ChangeOwner)
	case op.Expire != nil:
		return processExpire(cs, op.Expire)
	default:
		return fmt.Errorf("empty operation")
	}
}

// flushEntities encodes every dirty entity once and calls SetCode. Called once
// per block from CacheStore.Commit(), before stateDB.Commit().
func (c *CacheStore) flushEntities() error {
	for addr, entity := range c.dirtyEntities {
		data, _, err := encodeEntity(*entity)
		if err != nil {
			return err
		}
		c.stateDB.SetCode(addr, data, tracing.CodeChangeUnspecified)
	}
	return nil
}

// processCreate applies a Create operation.
func processCreate(cs *CacheStore, op *types.CreateOp) error {
	addr := common.Address(op.EntityKey[:20])

	// 1. Assign entity ID and set up the trie account.
	entityID := incrementEntityCount(cs.stateDB)
	cs.stateDB.CreateAccount(addr)
	setIDSlot(cs.stateDB, entityID, addr)

	// 2. Write PebbleDB ID/addr mappings and journal them (mutable; explicit revert).
	idK := idKey(entityID)
	addrK := addrKey(addr)
	oldID, _ := cs.stagingDB.Get(idK)
	oldAddr, _ := cs.stagingDB.Get(addrK)
	cs.journal.record(idK, oldID)
	cs.journal.record(addrK, oldAddr)
	if err := cs.stagingDB.Put(idK, addr.Bytes()); err != nil {
		return err
	}
	if err := cs.stagingDB.Put(addrK, encodeUint64(entityID)); err != nil {
		return err
	}

	// 3. Build Entity and cache it; SetCode is deferred to flushEntities.
	entity := &Entity{
		Payload:        []byte(op.Payload),
		Owner:          op.Owner,
		Creator:        op.Sender,
		ExpiresAt:      uint64(op.ExpiresAt),
		CreatedAtBlock: cs.blockNumber,
		ContentType:    op.ContentType,
		Key:            op.EntityKey,
	}
	for _, a := range op.Annotations {
		if a.StringValue != nil {
			entity.StringAnnotations = append(entity.StringAnnotations, stringAnnot{a.Key, *a.StringValue})
		} else if a.NumericValue != nil {
			entity.NumericAnnotations = append(entity.NumericAnnotations, numericAnnot{a.Key, *a.NumericValue})
		}
	}
	cs.dirtyEntities[addr] = entity

	// 4. Update bitmap caches; blobs are deferred to flushBitmaps.
	for _, pair := range allAnnotations(*entity) {
		if err := bitmapAdd(cs, pair.key, pair.val, entityID); err != nil {
			return fmt.Errorf("bitmap add %q: %w", pair.key, err)
		}
	}

	return nil
}

// processUpdate applies an Update operation.
func processUpdate(cs *CacheStore, op *types.UpdateOp) error {
	addr := common.Address(op.EntityKey[:20])

	old, err := cs.getEntity(addr)
	if err != nil {
		return err
	}

	idBytes, err := cs.stagingDB.Get(addrKey(addr))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	updated := &Entity{
		Payload:        []byte(op.Payload),
		Owner:          old.Owner,
		Creator:        old.Creator,
		ExpiresAt:      uint64(op.ExpiresAt),
		CreatedAtBlock: old.CreatedAtBlock,
		ContentType:    op.ContentType,
		Key:            old.Key,
	}
	for _, a := range op.Annotations {
		if a.StringValue != nil {
			updated.StringAnnotations = append(updated.StringAnnotations, stringAnnot{a.Key, *a.StringValue})
		} else if a.NumericValue != nil {
			updated.NumericAnnotations = append(updated.NumericAnnotations, numericAnnot{a.Key, *a.NumericValue})
		}
	}

	oldSet := annotPairSet(allAnnotations(*old))
	newSet := annotPairSet(allAnnotations(*updated))

	for pair := range oldSet {
		if _, kept := newSet[pair]; !kept {
			if err := bitmapRemove(cs, pair.key, pair.val, entityID); err != nil {
				return fmt.Errorf("bitmap remove %q: %w", pair.key, err)
			}
		}
	}
	for pair := range newSet {
		if _, existed := oldSet[pair]; !existed {
			if err := bitmapAdd(cs, pair.key, pair.val, entityID); err != nil {
				return fmt.Errorf("bitmap add %q: %w", pair.key, err)
			}
		}
	}

	cs.dirtyEntities[addr] = updated
	return nil
}

// processDelete applies a Delete operation.
func processDelete(cs *CacheStore, op *types.DeleteOp) error {
	return deleteEntity(cs, common.Address(op.EntityKey[:20]))
}

func deleteEntity(cs *CacheStore, addr common.Address) error {
	entity, err := cs.getEntity(addr)
	if err != nil {
		return err
	}

	idBytes, err := cs.stagingDB.Get(addrKey(addr))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	for _, pair := range allAnnotations(*entity) {
		if err := bitmapRemove(cs, pair.key, pair.val, entityID); err != nil {
			return fmt.Errorf("bitmap remove %q: %w", pair.key, err)
		}
	}

	clearIDSlot(cs.stateDB, entityID)

	// Remove from dirty cache and clear trie code immediately so subsequent ops
	// in the same block see the entity as absent.
	delete(cs.dirtyEntities, addr)
	cs.stateDB.SetCode(addr, nil, tracing.CodeChangeUnspecified)
	return nil
}

// processExtend applies an Extend operation.
func processExtend(cs *CacheStore, op *types.ExtendOp) error {
	addr := common.Address(op.EntityKey[:20])

	entity, err := cs.getEntity(addr)
	if err != nil {
		return err
	}

	idBytes, err := cs.stagingDB.Get(addrKey(addr))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	if err := bitmapRemove(cs, "$expiration", numericVal(entity.ExpiresAt), entityID); err != nil {
		return err
	}
	entity.ExpiresAt = uint64(op.ExpiresAt)
	if err := bitmapAdd(cs, "$expiration", numericVal(entity.ExpiresAt), entityID); err != nil {
		return err
	}

	return nil
}

// processExpire removes an entity that has passed its expiration block.
// Semantically identical to processDelete from the store's perspective.
func processExpire(cs *CacheStore, op *types.ExpireOp) error {
	return deleteEntity(cs, common.Address(op.EntityKey[:20]))
}

// processChangeOwner applies a ChangeOwner operation.
func processChangeOwner(cs *CacheStore, op *types.ChangeOwnerOp) error {
	addr := common.Address(op.EntityKey[:20])

	entity, err := cs.getEntity(addr)
	if err != nil {
		return err
	}

	idBytes, err := cs.stagingDB.Get(addrKey(addr))
	if err != nil {
		return fmt.Errorf("entity ID not found: %w", err)
	}
	entityID := decodeUint64(idBytes)

	if err := bitmapRemove(cs, "$owner", strings.ToLower(entity.Owner.Hex()), entityID); err != nil {
		return err
	}
	entity.Owner = op.NewOwner
	if err := bitmapAdd(cs, "$owner", strings.ToLower(entity.Owner.Hex()), entityID); err != nil {
		return err
	}

	return nil
}
