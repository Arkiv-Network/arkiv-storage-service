package types

import (
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Annotation is a key/value pair attached to an entity.
// Exactly one of StringValue or NumericValue is set.
type Annotation struct {
	Key          string  `json:"key"`
	StringValue  *string `json:"stringValue,omitempty"`
	NumericValue *uint64 `json:"numericValue,omitempty"`
}

// ArkivBlockHeader is the subset of a sealed block header forwarded by the ExEx.
type ArkivBlockHeader struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
}

// ArkivBlock is a sealed block containing only the Arkiv transactions extracted
// from successful EntityRegistry calls. Blocks with no Arkiv activity are still
// forwarded with an empty Transactions list.
type ArkivBlock struct {
	Header       ArkivBlockHeader   `json:"header"`
	Transactions []ArkivTransaction `json:"transactions"`
}

// ArkivTransaction is a single EntityRegistry call with its decoded operations.
type ArkivTransaction struct {
	Hash       common.Hash      `json:"hash"`
	Index      uint32           `json:"index"`
	Sender     common.Address   `json:"sender"`
	Operations []ArkivOperation `json:"operations"`
}

// ArkivBlockRef identifies a block by number and hash. Used in revert payloads.
type ArkivBlockRef struct {
	Number hexutil.Uint64 `json:"number"`
	Hash   common.Hash    `json:"hash"`
}

// ArkivOperation is a discriminated union of the Arkiv operation types.
// Exactly one of the pointer fields is non-nil after unmarshaling.
type ArkivOperation struct {
	Create      *CreateOp
	Update      *UpdateOp
	Delete      *DeleteOp
	Extend      *ExtendOp
	ChangeOwner *ChangeOwnerOp
	Expire      *ExpireOp
}

func (o ArkivOperation) MarshalJSON() ([]byte, error) {
	switch {
	case o.Create != nil:
		type T struct {
			Type string `json:"type"`
			*CreateOp
		}
		return json.Marshal(T{Type: "create", CreateOp: o.Create})
	case o.Update != nil:
		type T struct {
			Type string `json:"type"`
			*UpdateOp
		}
		return json.Marshal(T{Type: "update", UpdateOp: o.Update})
	case o.Delete != nil:
		type T struct {
			Type string `json:"type"`
			*DeleteOp
		}
		return json.Marshal(T{Type: "delete", DeleteOp: o.Delete})
	case o.Extend != nil:
		type T struct {
			Type string `json:"type"`
			*ExtendOp
		}
		return json.Marshal(T{Type: "extend", ExtendOp: o.Extend})
	case o.ChangeOwner != nil:
		type T struct {
			Type string `json:"type"`
			*ChangeOwnerOp
		}
		return json.Marshal(T{Type: "transfer", ChangeOwnerOp: o.ChangeOwner})
	case o.Expire != nil:
		type T struct {
			Type string `json:"type"`
			*ExpireOp
		}
		return json.Marshal(T{Type: "expire", ExpireOp: o.Expire})
	default:
		return nil, fmt.Errorf("ArkivOperation: no operation set")
	}
}

func (o *ArkivOperation) UnmarshalJSON(data []byte) error {
	var typed struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &typed); err != nil {
		return err
	}
	switch typed.Type {
	case "create":
		o.Create = new(CreateOp)
		return json.Unmarshal(data, o.Create)
	case "update":
		o.Update = new(UpdateOp)
		return json.Unmarshal(data, o.Update)
	case "delete":
		o.Delete = new(DeleteOp)
		return json.Unmarshal(data, o.Delete)
	case "extend":
		o.Extend = new(ExtendOp)
		return json.Unmarshal(data, o.Extend)
	case "transfer", "changeOwner":
		o.ChangeOwner = new(ChangeOwnerOp)
		return json.Unmarshal(data, o.ChangeOwner)
	case "expire":
		o.Expire = new(ExpireOp)
		return json.Unmarshal(data, o.Expire)
	default:
		return fmt.Errorf("unknown operation type %q", typed.Type)
	}
}

type CreateOp struct {
	// EntityKey is the 32-byte key minted by the EntityRegistry contract:
	//   keccak256(chainId || registry || owner || nonce)
	// Forwarded directly from the EntityOperation log by the ExEx.
	// The trie account address is derived as EntityKey[:20].
	EntityKey   common.Hash    `json:"entityKey"`
	// Sender is populated from ArkivTransaction.Sender by processBlock; it is
	// not part of the wire format (the ExEx places sender at the tx level).
	Sender      common.Address `json:"-"`
	Payload     hexutil.Bytes  `json:"payload"`
	ContentType string         `json:"contentType"`
	// ExpiresAt is serialized as a hex string by the Rust ExEx ("0x...").
	ExpiresAt   hexutil.Uint64 `json:"expiresAt"`
	Owner       common.Address `json:"owner"`
	// Annotations are called "attributes" on the wire (Rust field name).
	Annotations []Annotation   `json:"attributes"`
}

type UpdateOp struct {
	EntityKey   common.Hash    `json:"entityKey"`
	Payload     hexutil.Bytes  `json:"payload"`
	ContentType string         `json:"contentType"`
	ExpiresAt   hexutil.Uint64 `json:"expiresAt"`
	// Annotations are called "attributes" on the wire (Rust field name).
	Annotations []Annotation   `json:"attributes"`
}

type DeleteOp struct {
	EntityKey common.Hash `json:"entityKey"`
}

type ExtendOp struct {
	EntityKey common.Hash    `json:"entityKey"`
	// ExpiresAt is the new absolute expiration block, serialized as hex ("0x...").
	ExpiresAt hexutil.Uint64 `json:"expiresAt"`
}

type ChangeOwnerOp struct {
	EntityKey common.Hash    `json:"entityKey"`
	NewOwner  common.Address `json:"newOwner"`
}

// ExpireOp removes an entity that has passed its expiration block.
// Wire type tag: "expire".
type ExpireOp struct {
	EntityKey common.Hash `json:"entityKey"`
}
