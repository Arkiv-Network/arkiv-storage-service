package types

import (
	"encoding/json"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

// Attribute is a v2 wire-format typed attribute.
// JSON shape: {"valueType": "uint|string|entityKey", "name": "...", "value": "0x..."}
type Attribute struct {
	ValueType string        `json:"valueType"`
	Name      string        `json:"name"`
	Value     hexutil.Bytes `json:"value"`
}

// ArkivBlockHeader is the subset of a sealed block header forwarded by the ExEx.
type ArkivBlockHeader struct {
	Number     hexutil.Uint64 `json:"number"`
	Hash       common.Hash    `json:"hash"`
	ParentHash common.Hash    `json:"parentHash"`
	// ChangesetHash is the rolling changeset hash as of the end of this block.
	// For blocks with operations it equals the last operation's changesetHash;
	// for empty blocks it carries forward from the previous non-empty block.
	// Zero when no operation has ever been recorded as of this block.
	ChangesetHash common.Hash `json:"changesetHash"`
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
	Create   *CreateOp
	Update   *UpdateOp
	Delete   *DeleteOp
	Extend   *ExtendOp
	Transfer *TransferOp
	Expire   *ExpireOp
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
	case o.Transfer != nil:
		type T struct {
			Type string `json:"type"`
			*TransferOp
		}
		return json.Marshal(T{Type: "transfer", TransferOp: o.Transfer})
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
	case "transfer":
		o.Transfer = new(TransferOp)
		return json.Unmarshal(data, o.Transfer)
	case "expire":
		o.Expire = new(ExpireOp)
		return json.Unmarshal(data, o.Expire)
	default:
		return fmt.Errorf("unknown operation type %q", typed.Type)
	}
}

type CreateOp struct {
	OpIndex uint32 `json:"opIndex"`
	// EntityKey is the 32-byte key minted by the EntityRegistry contract:
	//   keccak256(chainId || registry || owner || nonce)
	// The trie account address is derived as EntityKey[:20].
	EntityKey common.Hash `json:"entityKey"`
	// Sender and TxIndex are populated from the enclosing ArkivTransaction by
	// processBlock; they are not part of the wire format (the ExEx places them
	// at the tx level, since all ops in a tx share the same values).
	Sender        common.Address `json:"-"`
	TxIndex       uint32         `json:"-"`
	Owner         common.Address `json:"owner"`
	ExpiresAt     hexutil.Uint64 `json:"expiresAt"`
	EntityHash    common.Hash    `json:"entityHash"`
	ChangesetHash common.Hash    `json:"changesetHash"`
	Payload       hexutil.Bytes  `json:"payload"`
	ContentType   string         `json:"contentType"`
	Attributes    []Attribute    `json:"attributes"`
}

// UpdateOp replaces an entity's payload, content type, and attributes.
// Expiration is not changed by an update; use ExtendOp to change it.
type UpdateOp struct {
	OpIndex       uint32         `json:"opIndex"`
	EntityKey     common.Hash    `json:"entityKey"`
	Owner         common.Address `json:"owner"`
	EntityHash    common.Hash    `json:"entityHash"`
	ChangesetHash common.Hash    `json:"changesetHash"`
	Payload       hexutil.Bytes  `json:"payload"`
	ContentType   string         `json:"contentType"`
	Attributes    []Attribute    `json:"attributes"`
}

type DeleteOp struct {
	OpIndex       uint32         `json:"opIndex"`
	EntityKey     common.Hash    `json:"entityKey"`
	Owner         common.Address `json:"owner"`
	EntityHash    common.Hash    `json:"entityHash"`
	ChangesetHash common.Hash    `json:"changesetHash"`
}

type ExtendOp struct {
	OpIndex       uint32         `json:"opIndex"`
	EntityKey     common.Hash    `json:"entityKey"`
	Owner         common.Address `json:"owner"`
	ExpiresAt     hexutil.Uint64 `json:"expiresAt"`
	EntityHash    common.Hash    `json:"entityHash"`
	ChangesetHash common.Hash    `json:"changesetHash"`
}

// TransferOp changes the owner of an entity. Owner is the new owner (derived
// from the EntityOperation event's owner field at the time of the transfer).
type TransferOp struct {
	OpIndex       uint32         `json:"opIndex"`
	EntityKey     common.Hash    `json:"entityKey"`
	Owner         common.Address `json:"owner"`
	EntityHash    common.Hash    `json:"entityHash"`
	ChangesetHash common.Hash    `json:"changesetHash"`
}

type ExpireOp struct {
	OpIndex       uint32         `json:"opIndex"`
	EntityKey     common.Hash    `json:"entityKey"`
	Owner         common.Address `json:"owner"`
	EntityHash    common.Hash    `json:"entityHash"`
	ChangesetHash common.Hash    `json:"changesetHash"`
}
