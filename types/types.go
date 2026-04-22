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

// ArkivBlock is a sealed block containing only the Arkiv operations extracted
// from successful EntityRegistry transactions. Blocks with no Arkiv activity
// are still forwarded with an empty Operations list.
type ArkivBlock struct {
	Header     ArkivBlockHeader `json:"header"`
	Operations []ArkivOperation `json:"operations"`
}

// ArkivBlockRef identifies a block by number and hash. Used in revert payloads.
type ArkivBlockRef struct {
	Number hexutil.Uint64 `json:"number"`
	Hash   common.Hash    `json:"hash"`
}

// ArkivOperation is a discriminated union of the five Arkiv operation types.
// Exactly one of the pointer fields is non-nil after unmarshaling.
type ArkivOperation struct {
	Create      *CreateOp
	Update      *UpdateOp
	Delete      *DeleteOp
	Extend      *ExtendOp
	ChangeOwner *ChangeOwnerOp
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
	case "changeOwner":
		o.ChangeOwner = new(ChangeOwnerOp)
		return json.Unmarshal(data, o.ChangeOwner)
	default:
		return fmt.Errorf("unknown operation type %q", typed.Type)
	}
}

type CreateOp struct {
	TxSeq         uint32         `json:"txSeq"`
	OpSeq         uint32         `json:"opSeq"`
	EntityAddress common.Address `json:"entityAddress"`
	Sender        common.Address `json:"sender"`
	Payload       hexutil.Bytes  `json:"payload"`
	ContentType   string         `json:"contentType"`
	ExpiresAt     uint64         `json:"expiresAt"`
	Owner         common.Address `json:"owner"`
	Annotations   []Annotation   `json:"annotations"`
}

type UpdateOp struct {
	EntityAddress common.Address `json:"entityAddress"`
	Payload       hexutil.Bytes  `json:"payload"`
	ContentType   string         `json:"contentType"`
	ExpiresAt     uint64         `json:"expiresAt"`
	Annotations   []Annotation   `json:"annotations"`
}

type DeleteOp struct {
	EntityAddress common.Address `json:"entityAddress"`
}

type ExtendOp struct {
	EntityAddress common.Address `json:"entityAddress"`
	NewExpiresAt  uint64         `json:"newExpiresAt"`
}

type ChangeOwnerOp struct {
	EntityAddress common.Address `json:"entityAddress"`
	NewOwner      common.Address `json:"newOwner"`
}
