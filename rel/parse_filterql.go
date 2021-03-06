package rel

import (
	"fmt"
	"strconv"
	"strings"

	u "github.com/araddon/gou"

	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/lex"
)

// Parses Tokens and returns an request.
func ParseFilterQL(filter string) (*FilterStatement, error) {
	l := lex.NewFilterQLLexer(filter)
	m := filterQLParser{l: l, filterTokenPager: newFilterTokenPager(l), buildVm: false}
	return m.parseFilterStart()
}
func ParseFilterQLVm(filter string) (*FilterStatement, error) {
	l := lex.NewFilterQLLexer(filter)
	m := filterQLParser{l: l, filterTokenPager: newFilterTokenPager(l), buildVm: true}
	return m.parseFilterStart()
}
func ParseFilterSelect(query string) (*FilterSelect, error) {
	l := lex.NewFilterQLLexer(query)
	m := filterQLParser{l: l, filterTokenPager: newFilterTokenPager(l), buildVm: false}
	return m.parseSelectStart()
}

type (
	// TokenPager is responsible for determining end of current clause
	//   An interface used to allow Parser to be neutral to dialect
	filterTokenPager struct {
		*expr.LexTokenPager
		lastKw lex.TokenType
	}

	// Parser, stateful representation of parser
	filterQLParser struct {
		buildVm bool
		fs      *FilterStatement
		l       *lex.Lexer
		comment string
		*filterTokenPager
		firstToken lex.Token
	}
)

func newFilterTokenPager(l *lex.Lexer) *filterTokenPager {
	pager := expr.NewLexTokenPager(l)
	return &filterTokenPager{LexTokenPager: pager}
}

func (m *filterTokenPager) IsEnd() bool {
	return m.LexTokenPager.IsEnd()
}
func (m *filterTokenPager) ClauseEnd() bool {
	tok := m.Cur()
	switch tok.T {
	// List of possible tokens that would indicate a end to the current clause
	case lex.TokenEOF, lex.TokenEOS, lex.TokenLimit, lex.TokenWith, lex.TokenAlias:
		return true
	}
	return false
}

func (m *filterQLParser) parseFilterStart() (*FilterStatement, error) {
	m.comment = m.initialComment()
	m.firstToken = m.Cur()
	switch m.firstToken.T {
	case lex.TokenFilter, lex.TokenWhere:
		return m.parseFilter()
	}
	u.Warnf("Could not parse?  %v   peek=%v", m.l.RawInput(), m.l.PeekX(40))
	return nil, fmt.Errorf("Unrecognized request type: %v", m.l.PeekWord())
}

func (m *filterQLParser) parseSelectStart() (*FilterSelect, error) {
	m.comment = m.initialComment()
	m.firstToken = m.Cur()
	switch m.firstToken.T {
	case lex.TokenSelect:
		return m.parseSelect()
	}
	u.Warnf("Could not parse?  %v   peek=%v", m.l.RawInput(), m.l.PeekX(40))
	return nil, fmt.Errorf("Unrecognized request type: %v", m.l.PeekWord())
}

func (m *filterQLParser) initialComment() string {

	comment := ""

	for {
		// We are going to loop until we find the first Non-Comment Token
		switch m.Cur().T {
		case lex.TokenComment, lex.TokenCommentML:
			comment += m.Cur().V
		case lex.TokenCommentStart, lex.TokenCommentHash, lex.TokenCommentEnd, lex.TokenCommentSingleLine, lex.TokenCommentSlashes:
			// skip, currently ignore these
		default:
			// first non-comment token
			return comment
		}
		m.Next()
	}
	return comment
}

func (m *filterQLParser) discardNewLines() {
	for {
		// We are going to loop until we find the first Non-NewLine
		switch m.Cur().T {
		case lex.TokenNewLine:
			m.Next()
		default:
			// first non-comment token
			return
		}
	}
}

// First keyword was SELECT, so use the SELECT parser rule-set
func (m *filterQLParser) parseSelect() (*FilterSelect, error) {

	req := &FilterSelect{FilterStatement: &FilterStatement{}}
	m.fs = req.FilterStatement
	req.Raw = m.l.RawInput()
	req.Description = m.comment
	m.Next() // Consume Select

	if err := parseColumns(m, nil, m.buildVm, req); err != nil {
		u.Debug(err)
		return nil, err
	}

	// OPTIONAL From clause
	if m.Cur().T == lex.TokenFrom {
		m.Next()
		if m.Cur().T == lex.TokenIdentity || m.Cur().T == lex.TokenTable {
			req.From = m.Next().V
		} else {
			return nil, fmt.Errorf("Expected FROM <identity> got %v", m.Cur())
		}
	}

	switch t := m.Next().T; t {
	case lex.TokenWhere:
		// one top level filter which may be nested
		if err := m.parseWhereExpr(req); err != nil {
			u.Debug(err)
			return nil, err
		}
	case lex.TokenFilter:
		//u.Warnf("starting filter %s", req.Raw)
		// one top level filter which may be nested
		filter, err := m.parseFirstFilters()
		if err != nil {
			u.Warnf("Could not parse filters %q err=%v", req.Raw, err)
			return nil, err
		}
		req.Filter = filter
	default:
		return nil, fmt.Errorf("expected SELECT * FROM <table> { <WHERE> | <FILTER> } but got %v instead of WHERE/FILTER", t)
	}

	m.discardNewLines()

	// LIMIT
	if limit, err := m.parseLimit(); err != nil {
		return nil, err
	} else {
		req.Limit = limit
	}

	// WITH
	with, err := ParseWith(m)
	if err != nil {
		return nil, err
	}
	req.With = with

	// ALIAS
	if alias, err := m.parseAlias(); err != nil {
		return nil, err
	} else {
		req.Alias = alias
	}

	if m.Cur().T == lex.TokenEOF || m.Cur().T == lex.TokenEOS || m.Cur().T == lex.TokenRightParenthesis {
		return req, nil
	}

	u.Warnf("Could not complete parsing, return error: %v %v", m.Cur(), m.l.PeekWord())
	return nil, fmt.Errorf("Did not complete parsing input: %v", m.LexTokenPager.Cur().V)
}

// First keyword was FILTER, so use the FILTER parser rule-set
func (m *filterQLParser) parseFilter() (*FilterStatement, error) {

	req := NewFilterStatement()
	m.fs = req
	req.Description = m.comment
	req.Raw = m.l.RawInput()
	m.Next() // Consume (FILTER | WHERE )

	// one top level filter which may be nested
	filter, err := m.parseFirstFilters()
	if err != nil {
		u.Warnf("Could not parse filters %q err=%v", req.Raw, err)
		return nil, err
	}
	m.discardNewLines()
	req.Filter = filter

	// OPTIONAL From clause
	if m.Cur().T == lex.TokenFrom {
		m.Next()
		if m.Cur().T != lex.TokenIdentity {
			return nil, fmt.Errorf("expected identity after FROM")
		}
		if m.Cur().T == lex.TokenIdentity || m.Cur().T == lex.TokenTable {
			req.From = m.Cur().V
			m.Next()
		}
	}

	// LIMIT
	if limit, err := m.parseLimit(); err != nil {
		return nil, err
	} else {
		req.Limit = limit
	}

	// ALIAS
	if alias, err := m.parseAlias(); err != nil {
		return nil, err
	} else {
		req.Alias = alias
	}

	// WITH
	with, err := ParseWith(m)
	if err != nil {
		return nil, err
	}
	req.With = with

	if m.Cur().T == lex.TokenEOF || m.Cur().T == lex.TokenEOS || m.Cur().T == lex.TokenRightParenthesis {
		// we are good
		return req, nil
	}

	u.Warnf("Could not complete parsing, return error: %v %v", m.Cur(), m.l.PeekWord())
	return nil, fmt.Errorf("Did not complete parsing input: %v", m.LexTokenPager.Cur().V)
}

func (m *filterQLParser) parseWhereExpr(req *FilterSelect) error {
	tree := expr.NewTree(m.filterTokenPager)
	if err := m.parseNode(tree); err != nil {
		u.Errorf("could not parse: %v", err)
		return err
	}

	fe := FilterExpr{Expr: tree.Root}
	filters := Filters{Op: lex.TokenAnd, Filters: []*FilterExpr{&fe}}
	req.Filter = &filters
	return nil
}

func (m *filterQLParser) parseFirstFilters() (*Filters, error) {

	//u.Infof("outer loop:  Cur():%v  %s", m.Cur(), m.l.RawInput())

	switch m.Cur().T {
	case lex.TokenStar, lex.TokenMultiply:

		m.Next() // Consume *
		filters := NewFilters(lex.TokenLogicAnd)
		fe := NewFilterExpr()
		fe.MatchAll = true
		filters.Filters = append(filters.Filters, fe)
		// if we have match all, nothing else allowed
		return filters, nil

	case lex.TokenIdentity:
		if strings.ToLower(m.Cur().V) == "match_all" {
			m.Next()
			filters := NewFilters(lex.TokenLogicAnd)
			fe := NewFilterExpr()
			fe.MatchAll = true
			filters.Filters = append(filters.Filters, fe)
			// if we have match all, nothing else allowed
			return filters, nil
		}
		// Fall through
	case lex.TokenNewLine:
		m.Next()
		return m.parseFirstFilters()
	}

	var op *lex.Token
	//u.Infof("cur? %#v", m.Cur())
	switch m.Cur().T {
	case lex.TokenAnd, lex.TokenOr, lex.TokenLogicAnd, lex.TokenLogicOr:
		op = &lex.Token{T: m.Cur().T, V: m.Cur().V}
		//found = true
		m.Next()
	}
	// If we don't have a shortcut
	filters, err := m.parseFilters(0, false, op)
	if err != nil {
		return nil, err
	}
	switch m.Cur().T {
	case lex.TokenRightParenthesis:
		u.Infof("consume right token")
		m.Next()
	}
	return filters, nil
}

func (m *filterQLParser) parseFilters(depth int, filtersNegate bool, filtersOp *lex.Token) (*Filters, error) {

	filters := NewFilters(lex.TokenLogicAnd) // Default outer is AND
	filters.Negate = filtersNegate
	if filtersOp != nil {
		filters.Op = filtersOp.T
		//u.Infof("%d %p setting filtersOp: %v", depth, filters, filters.String())
	}

	//u.Debugf("%d parseFilters() negate?%v filterop:%v cur:%v peek:%q", depth, filtersNegate, filtersOp, m.Cur(), m.l.PeekX(20))

	for {

		negate := false
		//found := false
		var op *lex.Token
		switch m.Cur().T {
		case lex.TokenNegate:
			negate = true
			//found = true
			m.Next()
		}

		switch m.Cur().T {
		case lex.TokenAnd, lex.TokenOr, lex.TokenLogicAnd, lex.TokenLogicOr:
			op = &lex.Token{T: m.Cur().T, V: m.Cur().V}
			//found = true
			m.Next()
		}
		//u.Debugf("%d start negate:%v  op:%v  filtersOp?%#v cur:%v", depth, negate, op, filtersOp, m.Cur())

		switch m.Cur().T {
		case lex.TokenLeftParenthesis:

			m.Next() // Consume   (

			if op == nil && filtersOp != nil && len(filters.Filters) == 0 {
				op = filtersOp
			}
			//u.Infof("%d %p consume ( op:%s for %s", depth, filters, op, filters.String())
			innerf, err := m.parseFilters(depth+1, negate, op)
			if err != nil {
				return nil, err
			}
			fe := NewFilterExpr()
			fe.Filter = innerf
			//u.Infof("%d inner ops:%s len=%d ql=%s", depth, filters.Op, len(innerf.Filters), innerf.String())
			filters.Filters = append(filters.Filters, fe)
			//u.Infof("%d %p filters: %s", depth, filters, filters.String())

		case lex.TokenUdfExpr, lex.TokenIdentity, lex.TokenLike, lex.TokenExists, lex.TokenBetween,
			lex.TokenIN, lex.TokenIntersects, lex.TokenValue, lex.TokenInclude, lex.TokenContains:

			if op != nil {
				u.Errorf("should not have op on Clause? %v", m.Cur())
			}
			fe, err := m.parseFilterClause(depth, negate)
			if err != nil {
				return nil, err
			}
			//u.Infof("%d adding %s   new: %v", depth, filters.String(), fe.String())
			filters.Filters = append(filters.Filters, fe)

		}
		//u.Debugf("after filter start?:   %v  ", m.Cur())

		// since we can loop inside switch statement
		switch m.Cur().T {
		case lex.TokenLimit, lex.TokenFrom, lex.TokenAlias, lex.TokenWith, lex.TokenEOS, lex.TokenEOF:
			return filters, nil
		case lex.TokenCommentSingleLine, lex.TokenCommentStart, lex.TokenCommentSlashes, lex.TokenComment,
			lex.TokenCommentEnd:
			// should we save this into filter?
			m.Next()
		case lex.TokenRightParenthesis:
			// end of this filter expression
			//u.Debugf("%d end ) %q", depth, filters.String())
			m.Next()
			return filters, nil
		case lex.TokenComma, lex.TokenNewLine:
			// keep looping, looking for more expressions
			m.Next()
		default:
			u.Warnf("cur? %v", m.Cur())
			return nil, fmt.Errorf("expected column but got: %v", m.Cur().String())
		}

		// reset any filter level stuff
		filtersNegate = false
		filtersOp = nil

	}
	//u.Debugf("filters: %d", len(filters.Filters))
	return filters, nil
}

func (m *filterQLParser) parseFilterClause(depth int, negate bool) (*FilterExpr, error) {

	fe := NewFilterExpr()
	fe.Negate = negate
	//u.Debugf("%d filterclause? negate?%v  cur=%v", depth, negate, m.Cur())

	switch m.Cur().T {
	case lex.TokenInclude:
		// embed/include a named filter

		m.Next()
		//u.Infof("type %v", m.Cur())
		if m.Cur().T != lex.TokenIdentity && m.Cur().T != lex.TokenValue {
			return nil, fmt.Errorf("Expected identity for Include but got %v", m.Cur())
		}
		fe.Include = m.Cur().V
		m.Next()

	case lex.TokenUdfExpr:
		// we have a udf/functional expression filter
		tree := expr.NewTree(m.filterTokenPager)
		if err := m.parseNode(tree); err != nil {
			u.Errorf("could not parse: %v", err)
			return nil, err
		}
		fe.Expr = tree.Root

	case lex.TokenIdentity, lex.TokenLike, lex.TokenExists, lex.TokenBetween,
		lex.TokenIN, lex.TokenIntersects, lex.TokenValue, lex.TokenContains:

		if m.Cur().T == lex.TokenIdentity {
			if strings.ToLower(m.Cur().V) == "include" {
				// TODO:  this is a bug in lexer ...
				// embed/include a named filter
				m.Next()
				if m.Cur().T != lex.TokenIdentity && m.Cur().T != lex.TokenValue {
					return nil, fmt.Errorf("Expected identity for Include but got %v", m.Cur())
				}
				fe.Include = m.Cur().V
				m.Next()
				return fe, nil
			}
		}

		tree := expr.NewTree(m.filterTokenPager)
		if err := m.parseNode(tree); err != nil {
			u.Errorf("could not parse: %v", err)
			return nil, err
		}
		fe.Expr = tree.Root
		if !m.fs.HasDateMath {
			m.fs.HasDateMath = expr.HasDateMath(fe.Expr)
		}
	default:
		return nil, fmt.Errorf("Expected clause but got %v", m.Cur())
	}
	return fe, nil
}

// Parse an expression tree or root Node
func (m *filterQLParser) parseNode(tree *expr.Tree) error {
	//u.Debugf("cur token parse: token=%v", m.Cur())
	err := tree.BuildTree(m.buildVm)
	if err != nil {
		u.Errorf("error: %v", err)
	}
	return err
}

func (m *filterQLParser) parseLimit() (int, error) {
	if m.Cur().T != lex.TokenLimit {
		return 0, nil
	}
	m.Next()
	if m.Cur().T != lex.TokenInteger {
		return 0, fmt.Errorf("Limit must be an integer %v", m.Cur())
	}
	iv, err := strconv.Atoi(m.Next().V)
	if err != nil {
		return 0, fmt.Errorf("Could not convert limit to integer %v", m.Cur().V)
	}

	return int(iv), nil
}

func (m *filterQLParser) parseAlias() (string, error) {
	if m.Cur().T != lex.TokenAlias {
		return "", nil
	}
	m.Next() // Consume ALIAS token
	if m.Cur().T != lex.TokenIdentity && m.Cur().T != lex.TokenValue {
		return "", fmt.Errorf("Expected identity but got: %v", m.Cur().T.String())
	}
	return strings.ToLower(m.Next().V), nil
}

func (m *filterQLParser) isEnd() bool {
	return m.IsEnd()
}
