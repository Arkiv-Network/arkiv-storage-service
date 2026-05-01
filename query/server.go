package query

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Arkiv-Network/arkiv-storage-service/store"
	"github.com/RoaringBitmap/roaring/v2/roaring64"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/rpc"
)

const queryResultCountLimit uint64 = 200

// IncludeData controls which fields are populated in each EntityData result.
// The zero value includes nothing; nil Options defaults to including everything.
type IncludeData struct {
	Key                         bool `json:"key"`
	Attributes                  bool `json:"attributes"`
	SyntheticAttributes         bool `json:"syntheticAttributes"`
	Payload                     bool `json:"payload"`
	ContentType                 bool `json:"contentType"`
	Expiration                  bool `json:"expiration"`
	Owner                       bool `json:"owner"`
	CreatedAtBlock              bool `json:"createdAtBlock"`
	LastModifiedAtBlock         bool `json:"lastModifiedAtBlock"`
	TransactionIndexInBlock     bool `json:"transactionIndexInBlock"`
	OperationIndexInTransaction bool `json:"operationIndexInTransaction"`
}

// Options holds per-request query parameters.
type Options struct {
	// AtBlock specifies the block number to query at. Only the canonical head
	// is currently supported (nil or 0); a future block-number→hash index would
	// allow arbitrary historical queries.
	AtBlock        *hexutil.Uint64 `json:"atBlock,omitempty"`
	IncludeData    *IncludeData    `json:"includeData,omitempty"`
	ResultsPerPage *hexutil.Uint64 `json:"resultsPerPage,omitempty"`
	// Cursor is the hex-encoded entity ID of the last item on the previous page
	// (hexutil.EncodeUint64). The next page contains IDs strictly below the cursor.
	Cursor string `json:"cursor,omitempty"`
}

func (o *Options) getResultsPerPage() uint64 {
	if o == nil || o.ResultsPerPage == nil || uint64(*o.ResultsPerPage) > queryResultCountLimit {
		return queryResultCountLimit
	}
	return uint64(*o.ResultsPerPage)
}

func (o *Options) getIncludeData() IncludeData {
	if o == nil || o.IncludeData == nil {
		return IncludeData{
			Key:                 true,
			ContentType:         true,
			Payload:             true,
			Owner:               true,
			Attributes:          true,
			Expiration:          true,
			CreatedAtBlock:      true,
			LastModifiedAtBlock: true,
		}
	}
	return *o.IncludeData
}

func (o *Options) getCursor() (*uint64, error) {
	if o == nil || o.Cursor == "" {
		return nil, nil
	}
	v, err := hexutil.DecodeUint64(o.Cursor)
	if err != nil {
		return nil, fmt.Errorf("error decoding cursor: %w", err)
	}
	return &v, nil
}

// QueryResponse is the JSON-RPC response for arkiv_query.
type QueryResponse struct {
	Data        []json.RawMessage `json:"data"`
	BlockNumber hexutil.Uint64    `json:"blockNumber"`
	Cursor      *string           `json:"cursor,omitempty"`
}

// EntityData holds the fields returned for each matched entity.
type EntityData struct {
	Key                         *common.Hash    `json:"key,omitempty"`
	Value                       hexutil.Bytes   `json:"value,omitempty"`
	ContentType                 *string         `json:"contentType,omitempty"`
	ExpiresAt                   *uint64         `json:"expiresAt,omitempty"`
	Owner                       *common.Address `json:"owner,omitempty"`
	CreatedAtBlock              *uint64         `json:"createdAtBlock,omitempty"`
	LastModifiedAtBlock         *uint64         `json:"lastModifiedAtBlock,omitempty"`
	TransactionIndexInBlock     *uint64         `json:"transactionIndexInBlock,omitempty"`
	OperationIndexInTransaction *uint64         `json:"operationIndexInTransaction,omitempty"`

	StringAttributes  []Attribute[string] `json:"stringAttributes,omitempty"`
	NumericAttributes []Attribute[uint64] `json:"numericAttributes,omitempty"`
}

// Attribute is a typed key-value annotation pair.
type Attribute[T any] struct {
	Key   string `json:"key"`
	Value T      `json:"value"`
}

// Backend extends StoreQuerier with entity lookup and head access. *store.Store
// implements Backend.
type Backend interface {
	StoreQuerier
	HeadNumber() uint64
	IDToAddress(id uint64) (common.Address, bool)
	GetEntityBytes(atBlockNumber uint64, addr common.Address) ([]byte, error)
}

// handler implements the arkiv_* JSON-RPC methods for entity queries.
// Exported methods are registered as arkiv_<camelCaseMethodName>.
type handler struct {
	backend Backend
}

// Query evaluates a query string against the bitmap index and returns the
// matching entities. Registered as arkiv_query.
func (h *handler) Query(ctx context.Context, queryStr string, options *Options) (*QueryResponse, error) {
	_ = ctx

	ast, err := Parse(queryStr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse query: %w", err)
	}

	var atBlockNumber uint64
	if options != nil && options.AtBlock != nil {
		atBlockNumber = uint64(*options.AtBlock)
	}

	// Determine the block number to report in the response: the requested block
	// if specified, otherwise the current canonical head.
	headNumber := h.backend.HeadNumber()
	responseBlockNumber := atBlockNumber
	if responseBlockNumber == 0 {
		responseBlockNumber = headNumber
	}

	ids, err := ast.Evaluate(h.backend, atBlockNumber)
	if err != nil {
		return nil, fmt.Errorf("failed to evaluate query: %w", err)
	}

	cursor, err := options.getCursor()
	if err != nil {
		return nil, err
	}

	// Apply cursor: restrict to IDs strictly below the cursor value so that
	// the next page picks up where the previous page left off.
	if cursor != nil {
		mask := roaring64.New()
		mask.AddRange(0, *cursor)
		ids.And(mask)
	}

	maxResults := options.getResultsPerPage()
	include := options.getIncludeData()

	res := &QueryResponse{
		Data:        []json.RawMessage{},
		BlockNumber: hexutil.Uint64(responseBlockNumber),
	}

	it := ids.ReverseIterator()
	var lastID *uint64

	for it.HasNext() && uint64(len(res.Data)) < maxResults {
		batchSize := min(maxResults-uint64(len(res.Data)), 10)
		ids := make([]uint64, 0, batchSize)
		for range batchSize {
			if !it.HasNext() {
				break
			}
			ids = append(ids, it.Next())
		}

		for _, id := range ids {
			lastID = &id
			addr, ok := h.backend.IDToAddress(id)
			if !ok {
				continue
			}
			ed, err := h.fetchEntityData(atBlockNumber, addr, include)
			if err != nil {
				// Entity may have been deleted between bitmap read and fetch; skip.
				continue
			}
			d, err := json.Marshal(ed)
			if err != nil {
				return nil, fmt.Errorf("failed to marshal entity data: %w", err)
			}
			res.Data = append(res.Data, d)
		}
	}

	if it.HasNext() && lastID != nil {
		c := hexutil.EncodeUint64(*lastID)
		res.Cursor = &c
	}

	return res, nil
}

// GetEntityByAddress returns the entity data for the given account address.
// Returns nil if the entity does not exist. Registered as arkiv_getEntityByAddress.
func (h *handler) GetEntityByAddress(ctx context.Context, addr common.Address, options *Options) (*EntityData, error) {
	_ = ctx
	var atBlockNumber uint64
	if options != nil && options.AtBlock != nil {
		atBlockNumber = uint64(*options.AtBlock)
	}
	return h.fetchEntityData(atBlockNumber, addr, options.getIncludeData())
}

// GetEntityCount returns the total number of live entities at the head.
// Registered as arkiv_getEntityCount.
func (h *handler) GetEntityCount(ctx context.Context) (uint64, error) {
	_ = ctx
	bm, err := h.backend.AllEntities(0)
	if err != nil {
		return 0, err
	}
	return bm.GetCardinality(), nil
}

// fetchEntityData loads and decodes entity bytes at addr, populating only the
// fields requested by include.
func (h *handler) fetchEntityData(atBlockNumber uint64, addr common.Address, include IncludeData) (*EntityData, error) {
	data, err := h.backend.GetEntityBytes(atBlockNumber, addr)
	if err != nil {
		return nil, err
	}
	e, err := store.DecodeEntity(data)
	if err != nil {
		return nil, fmt.Errorf("decode entity at %s: %w", addr, err)
	}

	ed := &EntityData{}

	if include.Key {
		k := e.Key
		ed.Key = &k
	}
	if include.Payload {
		ed.Value = hexutil.Bytes(e.Payload)
	}
	if include.ContentType {
		ct := e.ContentType
		ed.ContentType = &ct
	}
	if include.Expiration {
		ed.ExpiresAt = &e.ExpiresAt
	}
	if include.Owner {
		owner := e.Owner
		ed.Owner = &owner
	}
	if include.CreatedAtBlock {
		ed.CreatedAtBlock = &e.CreatedAtBlock
	}
	if include.LastModifiedAtBlock {
		ed.LastModifiedAtBlock = &e.LastModifiedAtBlock
	}
	if include.TransactionIndexInBlock {
		ed.TransactionIndexInBlock = &e.TransactionIndexInBlock
	}
	if include.OperationIndexInTransaction {
		ed.OperationIndexInTransaction = &e.OperationIndexInTransaction
	}

	// Populate user-defined annotations and/or synthetic ($-prefixed) annotations.
	switch {
	case include.Attributes && include.SyntheticAttributes:
		ed.StringAttributes = sortedStringAttrs(e, anyAnnot)
		ed.NumericAttributes = sortedNumericAttrs(e, anyAnnot)
	case include.Attributes:
		ed.StringAttributes = sortedStringAttrs(e, userAnnot)
		ed.NumericAttributes = sortedNumericAttrs(e, userAnnot)
	case include.SyntheticAttributes:
		ed.StringAttributes = sortedStringAttrs(e, syntheticAnnot)
		ed.NumericAttributes = sortedNumericAttrs(e, syntheticAnnot)
	}

	return ed, nil
}

func syntheticAnnot(key string) bool { return strings.HasPrefix(key, "$") }
func userAnnot(key string) bool      { return !strings.HasPrefix(key, "$") }
func anyAnnot(_ string) bool         { return true }

func sortedStringAttrs(e store.Entity, keep func(string) bool) []Attribute[string] {
	var out []Attribute[string]
	for _, a := range e.StringAnnotations {
		if keep(a.Key) {
			out = append(out, Attribute[string]{Key: a.Key, Value: a.Value})
		}
	}
	// Synthetic string annotations derived from entity fields.
	if keep("$owner") {
		out = append(out, Attribute[string]{Key: "$owner", Value: e.Owner.Hex()})
	}
	if keep("$creator") {
		out = append(out, Attribute[string]{Key: "$creator", Value: e.Creator.Hex()})
	}
	if keep("$key") {
		out = append(out, Attribute[string]{Key: "$key", Value: e.Key.Hex()})
	}
	if keep("$contentType") && e.ContentType != "" {
		out = append(out, Attribute[string]{Key: "$contentType", Value: e.ContentType})
	}
	slices.SortFunc(out, func(a, b Attribute[string]) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func sortedNumericAttrs(e store.Entity, keep func(string) bool) []Attribute[uint64] {
	var out []Attribute[uint64]
	for _, a := range e.NumericAnnotations {
		if keep(a.Key) {
			out = append(out, Attribute[uint64]{Key: a.Key, Value: a.Value})
		}
	}
	// Synthetic numeric annotations derived from entity fields.
	if keep("$expiration") {
		out = append(out, Attribute[uint64]{Key: "$expiration", Value: e.ExpiresAt})
	}
	if keep("$createdAtBlock") {
		out = append(out, Attribute[uint64]{Key: "$createdAtBlock", Value: e.CreatedAtBlock})
	}
	slices.SortFunc(out, func(a, b Attribute[uint64]) int { return strings.Compare(a.Key, b.Key) })
	return out
}

// Server exposes the Arkiv query JSON-RPC 2.0 API over HTTP.
type Server struct {
	rpc *rpc.Server
}

// New creates a query Server backed by the given Backend.
func New(backend Backend) (*Server, error) {
	srv := rpc.NewServer()
	if err := srv.RegisterName("arkiv", &handler{backend: backend}); err != nil {
		return nil, err
	}
	return &Server{rpc: srv}, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.rpc.ServeHTTP(w, r)
}
