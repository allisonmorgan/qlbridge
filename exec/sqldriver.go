package exec

import (
	"bytes"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	u "github.com/araddon/gou"
	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
)

var (
	// Ensure our driver implements appropriate database/sql interfaces
	_ driver.Conn    = (*qlbConn)(nil)
	_ driver.Driver  = (*qlbdriver)(nil)
	_ driver.Execer  = (*qlbConn)(nil)
	_ driver.Queryer = (*qlbConn)(nil)
	_ driver.Result  = (*qlbResult)(nil)
	_ driver.Rows    = (*qlbRows)(nil)
	_ driver.Stmt    = (*qlbStmt)(nil)
	//_ driver.Tx      = (*driverConn)(nil)

	// Create an instance of our driver
	qlbd          = &qlbdriver{}
	qlbDriverOnce sync.Once

	// Runtime Config supplies Schema information, or
	//  which sources exist
	rtConf = datasource.NewRuntimeConfig()

	// hm
	_ = u.EMPTY
)

const (
	MysqlTimeFormat = "2006-01-02 15:04:05.000000000"
)

func RegisterSqlDriver() {
	qlbDriverOnce.Do(func() { sql.Register("qlbridge", qlbd) })
}

// sql.Driver Interface implementation.
//
// Notes about Value return types:
//     Value is a value that drivers must be able to handle.
//     It is either nil or an instance of one of these types:
//
//       int64
//       float64
//       bool
//       []byte
//       string   [*] everywhere except from Rows.Next.
//       time.Time
type qlbdriver struct{}

// Open returns a new connection to the database.
//
// Open may return a cached connection (one previously closed), but doing so
// is unnecessary; the sql package maintains a pool of idle connections for
// efficient re-use.
//
// The returned connection is only used by one goroutine at a time.
func (m *qlbdriver) Open(connInfo string) (driver.Conn, error) {
	u.Infof("qlbdriver.Open():  %v", connInfo)
	rtConf.SetConnInfo(connInfo)
	return &qlbConn{rtConf: rtConf, conn: connInfo}, nil
}

// sql.Conn Interface implementation.
//
//
// plus:
// Execer is an optional interface that may be implemented by a Conn.
//        If a Conn does not implement Execer, the sql package's DB.Exec will
//        first prepare a query, execute the statement, and then close the
//        statement.
//
// Queryer is an optional interface that may be implemented by a Conn.
//        If a Conn does not implement Queryer, the sql package's DB.Query will
//        first prepare a query, execute the statement, and then close the
//        statement.
type qlbConn struct {
	parallel bool // Do we Run In Background Mode?  Default = true
	rtConf   *datasource.RuntimeConfig
	conn     string
}

// Exec may return ErrSkip.
//
// Execer implementation. To be used for queries that do not return any rows
// such as Create Index, Insert, Upset, Delete etc
func (m *qlbConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	return nil, expr.ErrNotImplemented
}

// Queryer implementation
// Query may return ErrSkip
//
func (m *qlbConn) Query(query string, args []driver.Value) (driver.Rows, error) {
	stmt := &qlbStmt{conn: m, query: query}
	return stmt.Query(args)
}

// Prepare returns a prepared statement, bound to this connection.
func (m *qlbConn) Prepare(query string) (driver.Stmt, error) {
	return nil, expr.ErrNotImplemented
}

// Close invalidates and potentially stops any current
// prepared statements and transactions, marking this
// connection as no longer in use.
//
// Because the sql package maintains a free pool of
// connections and only calls Close when there's a surplus of
// idle connections, it shouldn't be necessary for drivers to
// do their own connection caching.
func (m *qlbConn) Close() error {
	u.Debugf("do we need to do anything here?   job.Close()?")
	return nil
}

// Begin starts and returns a new transaction.
func (m *qlbConn) Begin() (driver.Tx, error) {
	return nil, expr.ErrNotImplemented
}

// sql.Tx Interface implementation.
//
// Tx is a transaction.
type qlbTx struct{}

func (conn *qlbTx) Commit() error {
	return expr.ErrNotImplemented
}
func (conn *qlbTx) Rollback() error { return expr.ErrNotImplemented }

// driver.Stmt Interface implementation.
//
// Stmt is a prepared statement. It is bound to a Conn and not
// used by multiple goroutines concurrently.
//
type qlbStmt struct {
	job   *SqlJob
	query string
	conn  *qlbConn
}

// Close closes the statement.
//
// As of Go 1.1, a Stmt will not be closed if it's in use
// by any queries.
func (m *qlbStmt) Close() error {
	if m.job != nil {
		m.job.Close()
	}
	return nil
}

// NumInput returns the number of placeholder parameters.
//
// If NumInput returns >= 0, the sql package will sanity check
// argument counts from callers and return errors to the caller
// before the statement's Exec or Query methods are called.
//
// NumInput may also return -1, if the driver doesn't know
// its number of placeholders. In that case, the sql package
// will not sanity check Exec or Query argument counts.
func (m *qlbStmt) NumInput() int { return 0 }

// Exec executes a query that doesn't return rows, such
// as an INSERT or UPDATE.
func (m *qlbStmt) Exec(args []driver.Value) (driver.Result, error) { return nil, expr.ErrNotImplemented }

// Query executes a query that may return rows, such as a SELECT
func (m *qlbStmt) Query(args []driver.Value) (driver.Rows, error) {
	var err error
	if len(args) > 0 {
		m.query, err = queryArgsConvert(m.query, args)
		if err != nil {
			return nil, err
		}
	}
	u.Infof("query: %v", m.query)

	// Create a Job, which is Dag of Tasks that Run()
	job, err := BuildSqlJob(m.conn.rtConf, m.conn.conn, m.query)
	if err != nil {
		return nil, err
	}
	m.job = job

	// The only type of stmt that makes sense for Query is SELECT
	//  and we need list of columns that requires casing
	sqlSelect, ok := job.Stmt.(*expr.SqlSelect)
	if !ok {
		return nil, fmt.Errorf("We could not recognize that as a select query: %T", job.Stmt)
	}

	// Prepare a result writer, we manually append this task to end
	// of job?
	resultWriter := NewResultRows(sqlSelect.Columns.FieldNames())

	job.Tasks.Add(resultWriter)

	job.Setup()

	// TODO:   this can't run in parallel-buffered mode?
	// how to open in go-routine and still be able to send error to rows?
	go func() {
		u.Debugf("Start Job.Run")
		err = job.Run()
		u.Debugf("After job.Run()")
		if err != nil {
			u.Errorf("error on Query.Run(): %v", err)
			//resultWriter.ErrChan() <- err
			//job.Close()
		}
		//job.Close()
		//u.Debugf("exiting Background Query")
	}()

	return resultWriter, nil
}

// driver.ColumnConverter Interface implementation.
//
// ColumnConverter may be optionally implemented by driver.Stmt if the
// statement is aware of its own columns' types and can convert from
// any type to a driver Value.
//
// ColumnConverter returns a ValueConverter for the provided
// column index.  If the type of a specific column isn't known
// or shouldn't be handled specially, DefaultValueConverter
// can be returned.
func (conn *qlbStmt) ColumnConverter(idx int) driver.ValueConverter { return nil }

// driver.Rows Interface implementation.
//
// Rows is an iterator over an executed query's results.
//
type qlbRows struct{}

// Columns returns the names of the columns. The number of
// columns of the result is inferred from the length of the
// slice.  If a particular column name isn't known, an empty
// string should be returned for that entry.
func (conn *qlbRows) Columns() []string { return nil }

// Close closes the rows iterator.
func (conn *qlbRows) Close() error { return expr.ErrNotImplemented }

// Next is called to populate the next row of data into
// the provided slice. The provided slice will be the same
// size as the Columns() are wide.
//
// The dest slice may be populated only with
// a driver Value type, but excluding string.
// All string values must be converted to []byte.
//
// Next should return io.EOF when there are no more rows.
func (conn *qlbRows) Next(dest []driver.Value) error { return expr.ErrNotImplemented }

// driver.Result Interface implementation.
//
// Result is the result of a query execution.
//
type qlbResult struct{}

// LastInsertId returns the database's auto-generated ID
// after, for example, an INSERT into a table with primary
// key.
func (conn *qlbResult) LastInsertId() (int64, error) { return 0, expr.ErrNotImplemented }

// RowsAffected returns the number of rows affected by the
// query.
func (conn *qlbResult) RowsAffected() (int64, error) { return 0, expr.ErrNotImplemented }

func join(a []string) string {
	n := 0
	for _, s := range a {
		n += len(s)
	}
	b := make([]byte, n)
	n = 0
	for _, s := range a {
		n += copy(b[n:], s)
	}
	return string(b)
}

func queryArgsConvert(query string, args []driver.Value) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	// a tiny, tiny, tiny bit of string sanitization
	if strings.ContainsAny(query, `'"`) {
		return "", nil
	}
	q := make([]string, 2*len(args)+1)
	n := 0
	for _, a := range args {
		i := strings.IndexRune(query, '?')
		if i == -1 {
			return "", errors.New("number of parameters doesn't match number of placeholders")
		}
		var s string
		switch v := a.(type) {
		case nil:
			s = "NULL"
		case string:
			s = "'" + escapeString(v) + "'"
		case []byte:
			s = "'" + escapeString(string(v)) + "'"
		case int64:
			s = strconv.FormatInt(v, 10)
		case time.Time:
			s = "'" + v.Format(MysqlTimeFormat) + "'"
		case bool:
			if v {
				s = "1"
			} else {
				s = "0"
			}
		case float64:
			s = strconv.FormatFloat(v, 'e', 12, 64)
		default:
			panic(fmt.Sprintf("%v (%T) can't be handled by godrv"))
		}
		q[n] = query[:i]
		q[n+1] = s
		query = query[i+1:]
		n += 2
	}
	q[n] = query
	return join(q), nil
}

func escapeString(txt string) string {
	var (
		esc string
		buf bytes.Buffer
	)
	last := 0
	for ii, bb := range txt {
		switch bb {
		case 0:
			esc = `\0`
		case '\n':
			esc = `\n`
		case '\r':
			esc = `\r`
		case '\\':
			esc = `\\`
		case '\'':
			esc = `\'`
		case '"':
			esc = `\"`
		case '\032':
			esc = `\Z`
		default:
			continue
		}
		io.WriteString(&buf, txt[last:ii])
		io.WriteString(&buf, esc)
		last = ii + 1
	}
	io.WriteString(&buf, txt[last:])
	return buf.String()
}

func escapeQuotes(txt string) string {
	var buf bytes.Buffer
	last := 0
	for ii, bb := range txt {
		if bb == '\'' {
			io.WriteString(&buf, txt[last:ii])
			io.WriteString(&buf, `''`)
			last = ii + 1
		}
	}
	io.WriteString(&buf, txt[last:])
	return buf.String()
}
