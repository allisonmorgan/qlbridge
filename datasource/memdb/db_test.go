package memdb

import (
	"database/sql/driver"
	"flag"
	"testing"
	"time"

	"github.com/araddon/dateparse"
	u "github.com/araddon/gou"
	"github.com/bmizerany/assert"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/schema"
)

func init() {
	flag.Parse()
	if testing.Verbose() {
		u.SetupLogging("debug")
		u.SetColorOutput()
	}
}

func TestMemDb(t *testing.T) {

	created, _ := dateparse.ParseAny("2015/07/04")

	db, err := NewMemDb("users", []string{"user_id", "name", "email", "created", "roles"})
	assert.Tf(t, err == nil, "wanted no error got %v", err)

	c, err := db.Open("users")
	assert.Tf(t, err == nil, "wanted no error got %v", err)
	dc, ok := c.(schema.ConnAll)
	assert.T(t, ok)

	dc.Put(nil, &datasource.KeyInt{123}, []driver.Value{123, "aaron", "email@email.com", created.In(time.UTC), []string{"admin"}})
	row, err := dc.Get(123)
	assert.T(t, err == nil)
	assert.Tf(t, row != nil, "Should find row with Get() part of Seeker interface")
	di, ok := row.(*datasource.SqlDriverMessage)
	assert.Tf(t, ok, "Must be []driver.Value type: %T", row)
	vals := di.Vals
	assert.Tf(t, len(vals) == 5, "want 5 cols in user but got %v", len(vals))
	assert.Tf(t, vals[0].(int) == 123, "want user_id=123 but got %v", vals[0])
	assert.Tf(t, vals[2].(string) == "email@email.com", "want email=email@email.com but got %v", vals[2])

	dc.Put(nil, &datasource.KeyInt{123}, []driver.Value{123, "aaron", "aaron@email.com", created.In(time.UTC), []string{"root", "admin"}})
	row, _ = dc.Get(123)
	assert.Tf(t, row != nil, "Should find row with Get() part of Seeker interface")
	vals2 := row.Body().([]driver.Value)

	assert.Tf(t, vals2[2].(string) == "aaron@email.com", "want email=email@email.com but got %v", vals2[2])
	assert.Equal(t, []string{"root", "admin"}, vals2[4], "Roles should match updated vals")
	assert.Equal(t, created, vals2[3], "created date should match updated vals")
}
