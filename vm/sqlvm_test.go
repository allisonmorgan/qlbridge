package vm

import (
	"testing"

	"github.com/araddon/dateparse"
	u "github.com/araddon/gou"
	"github.com/bmizerany/assert"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/rel"
	"github.com/araddon/qlbridge/value"
)

var (
	_ = u.EMPTY

	st1, _ = dateparse.ParseAny("12/18/2014")
	st2, _ = dateparse.ParseAny("12/18/2019")

	// This is the message context which will be added to all tests below
	//  and be available to the VM runtime for evaluation by using
	//  key's such as "int5" or "user_id"
	sqlData = datasource.NewContextSimpleData(map[string]value.Value{
		"int5":    value.NewIntValue(5),
		"str5":    value.NewStringValue("5"),
		"created": value.NewTimeValue(st1),
		"updated": value.NewTimeValue(st2),
		"bvalt":   value.NewBoolValue(true),
		"bvalf":   value.NewBoolValue(false),
		"user_id": value.NewStringValue("abc"),
		"urls":    value.NewStringsValue([]string{"abc", "123"}),
		"hits":    value.NewMapIntValue(map[string]int64{"google.com": 5, "bing.com": 1}),
		"email":   value.NewStringValue("bob@bob.com"),
	})
	sqlTestsX = []sqlTest{
		// Date math
		st(`select int5 FROM mycontext WHERE created < "now-1M"`, map[string]interface{}{"int5": 5}),
	}
	// list of tests
	sqlTests = []sqlTest{
		st(`select int5 FROM mycontext`, map[string]interface{}{"int5": 5}),
	}
)

func TestRunSqlTests(t *testing.T) {

	for _, test := range sqlTests {

		//u.Debugf("about to parse: %v", test.qlText)
		ss, err := rel.ParseSql(test.sql)
		assert.T(t, err == nil, "expected no error but got ", err, " for ", test.sql)

		sel, ok := ss.(*rel.SqlSelect)
		assert.Tf(t, ok, "expected rel.SqlSelect but got %T", ss)

		writeContext := datasource.NewContextSimple()
		_, err = EvalSql(sel, writeContext, test.context)
		assert.T(t, err == nil, "expected no error but got ", err, " for ", test.sql)

		for key, v := range test.result.Data {
			v2, ok := writeContext.Get(key)
			assert.Tf(t, ok, "Expected ok for get %s output: %#v", key, writeContext.Data)
			assert.Equalf(t, v2.Value(), v.Value(), "?? %s  %v!=%v %T %T", key, v.Value(), v2.Value(), v.Value(), v2.Value())
		}
	}
}

type sqlTest struct {
	sql     string
	context expr.ContextReader
	result  *datasource.ContextSimple // ?? what is this?
	rowct   int                       // expected row count
}

func st(sql string, results map[string]interface{}) sqlTest {
	return sqlTest{sql: sql, result: datasource.NewContextSimpleNative(results), context: sqlData}
}
