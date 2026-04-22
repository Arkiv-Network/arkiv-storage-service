package query

import (
	"github.com/alecthomas/participle/v2"
	"github.com/alecthomas/participle/v2/lexer"
)

const AnnotationIdentRegex string = `[\p{L}_][\p{L}\p{N}_]*`

// Define the lexer with distinct tokens for each operator and parentheses.
var lex = lexer.MustSimple([]lexer.SimpleRule{
	{Name: "Whitespace", Pattern: `[ \t\n\r]+`},
	{Name: "LParen", Pattern: `\(`},
	{Name: "RParen", Pattern: `\)`},
	{Name: "And", Pattern: `&&`},
	{Name: "Or", Pattern: `\|\|`},
	{Name: "Neq", Pattern: `!=`},
	{Name: "Eq", Pattern: `=`},
	{Name: "Geqt", Pattern: `>=`},
	{Name: "Leqt", Pattern: `<=`},
	{Name: "Gt", Pattern: `>`},
	{Name: "Lt", Pattern: `<`},
	{Name: "NotGlob", Pattern: `!~`},
	{Name: "Glob", Pattern: `~`},
	{Name: "Not", Pattern: `!`},
	{Name: "EntityKey", Pattern: `0x[a-fA-F0-9]{64}`},
	{Name: "Address", Pattern: `0x[a-fA-F0-9]{40}`},
	{Name: "String", Pattern: `"(?:[^"\\]|\\.)*"`},
	{Name: "Number", Pattern: `[0-9]+`},
	{Name: "Ident", Pattern: AnnotationIdentRegex},
	// Meta-annotations starting with $; longer patterns must come before shorter ones.
	{Name: "CreatedAtBlock", Pattern: `\$createdAtBlock`},
	{Name: "ContentType", Pattern: `\$contentType`},
	{Name: "Owner", Pattern: `\$owner`},
	{Name: "Creator", Pattern: `\$creator`},
	{Name: "Key", Pattern: `\$key`},
	{Name: "Expiration", Pattern: `\$expiration`},
	{Name: "Sequence", Pattern: `\$sequence`},
	{Name: "All", Pattern: `\$all`},
	{Name: "Star", Pattern: `\*`},
})

type TopLevel struct {
	Expression *Expression `parser:"@@ | All | Star"`
}

// Expression is the top-level rule.
type Expression struct {
	Or OrExpression `parser:"@@"`
}

// OrExpression handles expressions connected with ||.
type OrExpression struct {
	Left  AndExpression `parser:"@@"`
	Right []*OrRHS      `parser:"@@*"`
}

// OrRHS represents the right-hand side of an OR.
type OrRHS struct {
	Expr AndExpression `parser:"(Or | 'OR' | 'or') @@"`
}

// AndExpression handles expressions connected with &&.
type AndExpression struct {
	Left  EqualExpr `parser:"@@"`
	Right []*AndRHS `parser:"@@*"`
}

// AndRHS represents the right-hand side of an AND.
type AndRHS struct {
	Expr EqualExpr `parser:"(And | 'AND' | 'and') @@"`
}

// EqualExpr can be either an equality or a parenthesized expression.
type EqualExpr struct {
	Paren     *Paren     `parser:"  @@"`
	Assign    *Equality  `parser:"| @@"`
	Inclusion *Inclusion `parser:"| @@"`

	LessThan           *LessThan           `parser:"| @@"`
	LessOrEqualThan    *LessOrEqualThan    `parser:"| @@"`
	GreaterThan        *GreaterThan        `parser:"| @@"`
	GreaterOrEqualThan *GreaterOrEqualThan `parser:"| @@"`
	GlobExpr           *GlobExpr           `parser:"| @@"`
}

type Paren struct {
	IsNot  bool       `parser:"@(Not | 'NOT' | 'not')?"`
	Nested Expression `parser:"LParen @@ RParen"`
}

type GlobExpr struct {
	Var   string `parser:"@Ident"`
	IsNot bool   `parser:"((Glob | @NotGlob) | (@('NOT' | 'not')? ('GLOB' | 'glob')))"`
	Value string `parser:"@String"`
}

// Range query vars: user-defined idents plus numeric built-ins ($expiration, $createdAtBlock).
type LessThan struct {
	Var   string `parser:"@(Ident | Expiration | CreatedAtBlock) Lt"`
	Value Value  `parser:"@@"`
}

type LessOrEqualThan struct {
	Var   string `parser:"@(Ident | Expiration | CreatedAtBlock) Leqt"`
	Value Value  `parser:"@@"`
}

type GreaterThan struct {
	Var   string `parser:"@(Ident | Expiration | CreatedAtBlock) Gt"`
	Value Value  `parser:"@@"`
}

type GreaterOrEqualThan struct {
	Var   string `parser:"@(Ident | Expiration | CreatedAtBlock) Geqt"`
	Value Value  `parser:"@@"`
}

// Equality represents a simple equality (e.g. name = "foo" or $owner = 0x...).
type Equality struct {
	Var   string `parser:"@(Ident | Key | Owner | Creator | Expiration | Sequence | CreatedAtBlock | ContentType)"`
	IsNot bool   `parser:"(Eq | @Neq)"`
	Value Value  `parser:"@@"`
}

type Inclusion struct {
	Var    string `parser:"@(Ident | Key | Owner | Creator | Expiration | Sequence | CreatedAtBlock | ContentType)"`
	IsNot  bool   `parser:"(@('NOT'|'not')? ('IN'|'in'))"`
	Values Values `parser:"@@"`
}

// Value is a literal value (a number or a string).
type Value struct {
	String *string `parser:"  (@String | @EntityKey | @Address)"`
	Number *uint64 `parser:"| @Number"`
}

type Values struct {
	Strings []string `parser:"  '(' (@String | @EntityKey | @Address)+ ')'"`
	Numbers []uint64 `parser:"| '(' @Number+ ')'"`
}

var Parser = participle.MustBuild[TopLevel](
	participle.Lexer(lex),
	participle.Elide("Whitespace"),
	participle.Unquote("String"),
)

func Parse(s string) (*AST, error) {
	v, err := Parser.ParseString("", s)
	if err != nil {
		return nil, err
	}
	return v.Normalize(), nil
}
