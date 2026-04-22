package query

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
)

// StoreQuerier abstracts the store's bitmap query methods. The query package
// never manipulates StateDB or ethdb.Database directly; all access goes through
// this interface. *store.Store implements StoreQuerier.
type StoreQuerier interface {
	// ReadAnnotBitmap reads the trie-committed bitmap for (key, val) at the
	// given block number's state. atBlockNumber == 0 means the current canonical head.
	ReadAnnotBitmap(atBlockNumber uint64, key, val string) (*roaring64.Bitmap, error)
	// AllEntities returns the bitmap of all live entity IDs at the given block number.
	AllEntities(atBlockNumber uint64) (*roaring64.Bitmap, error)
	// IterateNumericAnnotBitmaps ORs bitmaps whose numeric value is in [lo, hi].
	// Scans the latest PebbleDB pointer index (not a historical trie state).
	IterateNumericAnnotBitmaps(key string, lo, hi uint64) *roaring64.Bitmap
	// IterateStringAnnotBitmaps ORs bitmaps whose string value is in [lo, hi].
	// Bounds are optional (nil = unbounded).
	IterateStringAnnotBitmaps(key string, lo *string, loIncl bool, hi *string, hiIncl bool) *roaring64.Bitmap
	// IteratePrefixAnnotBitmaps ORs bitmaps whose string value starts with prefix.
	IteratePrefixAnnotBitmaps(key, prefix string) *roaring64.Bitmap
	// IterateGlobAnnotBitmaps ORs bitmaps whose string value matches pattern.
	IterateGlobAnnotBitmaps(key, pattern string) *roaring64.Bitmap
}

// numericAnnotVal encodes a uint64 as 8-byte big-endian, matching the encoding
// used by the store for numeric annotation values.
func numericAnnotVal(v uint64) string {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return string(b[:])
}

// annotVal returns the bitmap lookup value for a parsed query Value.
// String values are used as-is; numeric values are encoded as 8-byte big-endian.
func annotVal(v *Value) string {
	if v.String != nil {
		return *v.String
	}
	return numericAnnotVal(*v.Number)
}

// isPrefixGlob reports whether pattern is a simple "prefix*" glob with no other
// wildcards. Returns the prefix if so.
func isPrefixGlob(pattern string) (string, bool) {
	if !strings.HasSuffix(pattern, "*") {
		return "", false
	}
	prefix := pattern[:len(pattern)-1]
	if strings.ContainsAny(prefix, "*?") {
		return "", false
	}
	return prefix, true
}

// Evaluate evaluates the parsed AST against the store bitmaps and returns the
// set of matching entity IDs. An empty AST (from * or $all) returns all live
// entity IDs.
func (t *AST) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	if t.Expr == nil {
		return q.AllEntities(atBlockNumber)
	}
	return t.Expr.Evaluate(q, atBlockNumber)
}

func (e *ASTExpr) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	return e.Or.Evaluate(q, atBlockNumber)
}

// Evaluate ORs together the results of each AND clause.
func (e *ASTOr) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	result := roaring64.New()
	for i := range e.Terms {
		bm, err := e.Terms[i].Evaluate(q, atBlockNumber)
		if err != nil {
			return nil, err
		}
		result.Or(bm)
	}
	return result, nil
}

// Evaluate ANDs together the results of each leaf term.
func (e *ASTAnd) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	var result *roaring64.Bitmap
	for i := range e.Terms {
		bm, err := e.Terms[i].Evaluate(q, atBlockNumber)
		if err != nil {
			return nil, err
		}
		if result == nil {
			result = bm
		} else {
			result.And(bm)
		}
	}
	if result == nil {
		return roaring64.New(), nil
	}
	return result, nil
}

func (e *ASTTerm) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	switch {
	case e.Assign != nil:
		return e.Assign.Evaluate(q, atBlockNumber)
	case e.Inclusion != nil:
		return e.Inclusion.Evaluate(q, atBlockNumber)
	case e.LessThan != nil:
		return e.LessThan.Evaluate(q)
	case e.LessOrEqualThan != nil:
		return e.LessOrEqualThan.Evaluate(q)
	case e.GreaterThan != nil:
		return e.GreaterThan.Evaluate(q)
	case e.GreaterOrEqualThan != nil:
		return e.GreaterOrEqualThan.Evaluate(q)
	case e.Glob != nil:
		return e.Glob.Evaluate(q, atBlockNumber)
	default:
		return nil, fmt.Errorf("unknown AST term: %v", e)
	}
}

// Evaluate performs a range scan for `var < n` (numeric) or `var < "s"` (string).
func (e *LessThan) Evaluate(q StoreQuerier) (*roaring64.Bitmap, error) {
	if e.Value.String != nil {
		return q.IterateStringAnnotBitmaps(e.Var, nil, false, e.Value.String, false), nil
	}
	n := *e.Value.Number
	if n == 0 {
		return roaring64.New(), nil
	}
	return q.IterateNumericAnnotBitmaps(e.Var, 0, n-1), nil
}

// Evaluate performs a range scan for `var <= n` (numeric) or `var <= "s"` (string).
func (e *LessOrEqualThan) Evaluate(q StoreQuerier) (*roaring64.Bitmap, error) {
	if e.Value.String != nil {
		return q.IterateStringAnnotBitmaps(e.Var, nil, false, e.Value.String, true), nil
	}
	return q.IterateNumericAnnotBitmaps(e.Var, 0, *e.Value.Number), nil
}

// Evaluate performs a range scan for `var > n` (numeric) or `var > "s"` (string).
func (e *GreaterThan) Evaluate(q StoreQuerier) (*roaring64.Bitmap, error) {
	if e.Value.String != nil {
		return q.IterateStringAnnotBitmaps(e.Var, e.Value.String, false, nil, false), nil
	}
	n := *e.Value.Number
	if n == math.MaxUint64 {
		return roaring64.New(), nil
	}
	return q.IterateNumericAnnotBitmaps(e.Var, n+1, math.MaxUint64), nil
}

// Evaluate performs a range scan for `var >= n` (numeric) or `var >= "s"` (string).
func (e *GreaterOrEqualThan) Evaluate(q StoreQuerier) (*roaring64.Bitmap, error) {
	if e.Value.String != nil {
		return q.IterateStringAnnotBitmaps(e.Var, e.Value.String, true, nil, false), nil
	}
	return q.IterateNumericAnnotBitmaps(e.Var, *e.Value.Number, math.MaxUint64), nil
}

// Evaluate performs an exact bitmap lookup.
// For IsNot, subtracts the matching set from all live entities.
func (e *Equality) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	bm, err := q.ReadAnnotBitmap(atBlockNumber, e.Var, annotVal(&e.Value))
	if err != nil {
		return nil, err
	}
	if e.IsNot {
		all, err := q.AllEntities(atBlockNumber)
		if err != nil {
			return nil, err
		}
		all.AndNot(bm)
		return all, nil
	}
	return bm, nil
}

// Evaluate resolves a glob match against PebbleDB bitmaps.
// For "prefix*" patterns it uses a range scan; all other patterns perform a
// full namespace scan filtered by pattern. For IsNot, the matching set is
// subtracted from all live entities.
func (e *GlobExpr) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	var bm *roaring64.Bitmap
	if prefix, ok := isPrefixGlob(e.Value); ok {
		bm = q.IteratePrefixAnnotBitmaps(e.Var, prefix)
	} else {
		bm = q.IterateGlobAnnotBitmaps(e.Var, e.Value)
	}
	if e.IsNot {
		all, err := q.AllEntities(atBlockNumber)
		if err != nil {
			return nil, err
		}
		all.AndNot(bm)
		return all, nil
	}
	return bm, nil
}

// Evaluate ORs the bitmaps for each candidate value, then optionally negates.
func (e *Inclusion) Evaluate(q StoreQuerier, atBlockNumber uint64) (*roaring64.Bitmap, error) {
	union := roaring64.New()

	if len(e.Values.Strings) > 0 {
		for _, s := range e.Values.Strings {
			bm, err := q.ReadAnnotBitmap(atBlockNumber, e.Var, s)
			if err != nil {
				return nil, err
			}
			union.Or(bm)
		}
	} else {
		for _, n := range e.Values.Numbers {
			bm, err := q.ReadAnnotBitmap(atBlockNumber, e.Var, numericAnnotVal(n))
			if err != nil {
				return nil, err
			}
			union.Or(bm)
		}
	}

	if e.IsNot {
		all, err := q.AllEntities(atBlockNumber)
		if err != nil {
			return nil, err
		}
		all.AndNot(union)
		return all, nil
	}
	return union, nil
}
