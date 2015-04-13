package exec

import (
	"database/sql/driver"
	"fmt"
	"net/url"
	"sync"
	//"time"

	u "github.com/araddon/gou"
	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
	"github.com/araddon/qlbridge/value"
	"github.com/araddon/qlbridge/vm"
	//"github.com/mdmarek/topo"
)

var (
	_ = u.EMPTY

	// Ensure that we implement the Task Runner interface
	// to ensure this can run in exec engine
	_ TaskRunner = (*Source)(nil)

	// Ensure that our source plan implements Subvisitor
	_ expr.SubVisitor = (*SourcePlan)(nil)
)

func NewSourcePlan(sql *expr.SqlSource) *SourcePlan {
	return &SourcePlan{SqlSource: sql}
}

type SourcePlan struct {
	SqlSource *expr.SqlSource
}

func (m *SourcePlan) Accept(sub expr.SubVisitor) (interface{}, error) {
	u.Debugf("Accept %+v", sub)
	return nil, expr.ErrNotImplemented
}
func (m *SourcePlan) VisitSubselect(stmt *expr.SqlSource) (interface{}, error) {
	u.Debugf("VisitSubselect %+v", stmt)
	return nil, expr.ErrNotImplemented
}

func (m *SourcePlan) VisitJoin(stmt *expr.SqlSource) (interface{}, error) {
	u.Debugf("VisitJoin %+v", stmt)
	return nil, expr.ErrNotImplemented
}

// Scan a data source for rows, feed into runner.  The source scanner being
//   a source is iter.Next() messages instead of sending them on input channel
//
//  1) table      -- FROM table
//  2) channels   -- FROM stream
//  3) join       -- SELECT t1.name, t2.salary
//                       FROM employee AS t1
//                       INNER JOIN info AS t2
//                       ON t1.name = t2.name;
//  4) sub-select -- SELECT * FROM (SELECT 1, 2, 3) AS t1;
//
type Source struct {
	*TaskBase
	from   *expr.SqlSource
	source datasource.Scanner
}

// A scanner to read from data source
func NewSource(from *expr.SqlSource, source datasource.Scanner) *Source {
	s := &Source{
		TaskBase: NewTaskBase("Source"),
		source:   source,
		from:     from,
	}
	s.TaskBase.TaskType = s.Type()
	return s
}

func (m *Source) Copy() *Source { return &Source{} }

func (m *Source) Close() error {
	if closer, ok := m.source.(datasource.DataSource); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if err := m.TaskBase.Close(); err != nil {
		return err
	}
	return nil
}

func (m *Source) Run(context *Context) error {
	defer context.Recover() // Our context can recover panics, save error msg
	defer close(m.msgOutCh) // closing input channels is the signal to stop

	// TODO:  Allow an alternate interface that allows Source to provide
	//        an output channel?
	scanner, ok := m.source.(datasource.Scanner)
	if !ok {
		return fmt.Errorf("Does not implement Scanner: %T", m.source)
	}
	//u.Debugf("scanner: %T %v", scanner, scanner)
	iter := scanner.CreateIterator(nil)
	//u.Debugf("iter in source: %T  %#v", iter, iter)

	for item := iter.Next(); item != nil; item = iter.Next() {

		//u.Infof("In source Scanner iter %#v", item)
		select {
		case <-m.SigChan():
			u.Warnf("got signal quit")
			return nil
		case m.msgOutCh <- item:
			// continue
		}

	}
	//u.Debugf("leaving source scanner")
	return nil
}

// Scan a data source for rows, feed into runner for join sources
//
//  1) join  SELECT t1.name, t2.salary
//               FROM employee AS t1
//               INNER JOIN info AS t2
//               ON t1.name = t2.name;
//
type SourceJoin struct {
	*TaskBase
	conf        *datasource.RuntimeConfig
	leftStmt    *expr.SqlSource
	rightStmt   *expr.SqlSource
	leftSource  datasource.Scanner
	rightSource datasource.Scanner
}

// A scanner to read from data source
func NewSourceJoin(builder expr.SubVisitor, leftFrom, rightFrom *expr.SqlSource, conf *datasource.RuntimeConfig) (*SourceJoin, error) {

	m := &SourceJoin{
		TaskBase: NewTaskBase("SourceJoin"),
	}
	m.TaskBase.TaskType = m.Type()

	m.leftStmt = leftFrom
	m.rightStmt = rightFrom

	u.Debugf("leftFrom.Name:'%v' : %v", leftFrom.Name, leftFrom.Source.StringAST())
	source := conf.Conn(leftFrom.Name)
	u.Debugf("left source: %T", source)
	// Must provider either Scanner, SourcePlanner, Seeker interfaces
	if sourcePlan, ok := source.(datasource.SourcePlanner); ok {
		//  This is flawed, visitor pattern would have you pass in a object which implements interface
		//    but is one of many different objects that implement that interface so that the
		//    Accept() method calls the apppropriate method
		op, err := sourcePlan.Accept(NewSourcePlan(leftFrom))
		// plan := NewSourcePlan(leftFrom)
		// op, err := plan.Accept(sourcePlan)
		if err != nil {
			u.Errorf("Could not source plan for %v  %T %#v", leftFrom.Name, source, source)
		}
		//u.Debugf("got op: %T  %#v", op, op)
		if scanner, ok := op.(datasource.Scanner); !ok {
			u.Errorf("Could not create scanner for %v  %T %#v", leftFrom.Name, op, op)
			return nil, fmt.Errorf("Must Implement Scanner")
		} else {
			m.leftSource = scanner
		}
	} else {
		if scanner, ok := source.(datasource.Scanner); !ok {
			u.Errorf("Could not create scanner for %v  %T %#v", leftFrom.Name, source, source)
			return nil, fmt.Errorf("Must Implement Scanner")
		} else {
			m.leftSource = scanner
		}
	}

	u.Debugf("right:  Name:'%v' : %v", rightFrom.Name, rightFrom.Source.String())
	source2 := conf.Conn(rightFrom.Name)
	u.Debugf("source right: %T", source2)
	// Must provider either Scanner, and or Seeker interfaces

	// Must provider either Scanner, SourcePlanner, Seeker interfaces
	if sourcePlan, ok := source2.(datasource.SourcePlanner); ok {
		//  This is flawed, visitor pattern would have you pass in a object which implements interface
		//    but is one of many different objects that implement that interface so that the
		//    Accept() method calls the apppropriate method
		op, err := sourcePlan.Accept(NewSourcePlan(rightFrom))
		// plan := NewSourcePlan(rightFrom)
		// op, err := plan.Accept(sourcePlan)
		if err != nil {
			u.Errorf("Could not source plan for %v  %T %#v", rightFrom.Name, source2, source2)
		}
		//u.Debugf("got op: %T  %#v", op, op)
		if scanner, ok := op.(datasource.Scanner); !ok {
			u.Errorf("Could not create scanner for %v  %T %#v", rightFrom.Name, op, op)
			return nil, fmt.Errorf("Must Implement Scanner")
		} else {
			m.rightSource = scanner
		}
	} else {
		if scanner, ok := source2.(datasource.Scanner); !ok {
			u.Errorf("Could not create scanner for %v  %T %#v", rightFrom.Name, source2, source2)
			return nil, fmt.Errorf("Must Implement Scanner")
		} else {
			m.rightSource = scanner
		}
	}

	return m, nil
}

func (m *SourceJoin) Copy() *Source { return &Source{} }

func (m *SourceJoin) Close() error {
	if closer, ok := m.leftSource.(datasource.DataSource); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if closer, ok := m.rightSource.(datasource.DataSource); ok {
		if err := closer.Close(); err != nil {
			return err
		}
	}
	if err := m.TaskBase.Close(); err != nil {
		return err
	}
	return nil
}

func (m *SourceJoin) Run(context *Context) error {
	defer context.Recover() // Our context can recover panics, save error msg
	defer close(m.msgOutCh) // closing input channels is the signal to stop

	//u.Infof("Run():  %T %#v", m.leftSource, m.leftSource)
	leftIn := m.leftSource.MesgChan(nil)
	rightIn := m.rightSource.MesgChan(nil)

	//u.Warnf("leftSource: %p  rightSource: %p", m.leftSource, m.rightSource)
	//u.Warnf("leftIn: %p  rightIn: %p", leftIn, rightIn)
	outCh := m.MessageOut()

	//u.Infof("Checking leftStmt:  %#v", m.leftStmt)
	//u.Infof("Checking rightStmt:  %#v", m.rightStmt)
	lhExpr, err := m.leftStmt.JoinValueExpr()
	if err != nil {
		return err
	}
	rhExpr, err := m.rightStmt.JoinValueExpr()
	if err != nil {
		return err
	}
	lcols := m.leftStmt.UnAliasedColumns()
	rcols := m.rightStmt.UnAliasedColumns()
	u.Infof("lcols:  %#v for sql %s", lcols, m.leftStmt.Source.String())
	u.Infof("rcols:  %#v for sql %v", rcols, m.rightStmt.Source.String())
	lh := make(map[string][]datasource.Message)
	rh := make(map[string][]datasource.Message)
	/*
			JOIN = INNER JOIN = Equal Join

			1)   we need to rewrite query for a source based on the Where + Join? + sort needed
			2)

		TODO:
			x get value for join ON to use in hash,  EvalJoinValues(msg) - this is similar to Projection?
			- manage the coordination of draining both/channels
			- evaluate hashes/output
	*/
	wg := new(sync.WaitGroup)
	wg.Add(1)
	go func() {
		for {
			//u.Infof("In source Scanner iter %#v", item)
			select {
			case <-m.SigChan():
				u.Warnf("got signal quit")
				return
			case msg, ok := <-leftIn:
				if !ok {
					u.Warnf("NICE, got left shutdown")
					wg.Done()
					return
				} else {

					if jv, ok := joinValue(nil, lhExpr, msg, lcols); ok {
						u.Debugf("left eval?:%v     %#v", jv, msg.Body())
						lh[jv] = append(lh[jv], msg)
					} else {
						u.Warnf("Could not evaluate? %v msg=%v", lhExpr.String(), msg.Body())
					}
				}
			}

		}
	}()
	wg.Add(1)
	go func() {
		for {

			//u.Infof("In source Scanner iter %#v", item)
			select {
			case <-m.SigChan():
				u.Warnf("got signal quit")
				return
			case msg, ok := <-rightIn:
				if !ok {
					u.Warnf("NICE, got right shutdown")
					wg.Done()
					return
				} else {
					if jv, ok := joinValue(nil, rhExpr, msg, rcols); ok {
						u.Debugf("right val:%v     %#v", jv, msg.Body())
						rh[jv] = append(rh[jv], msg)
					} else {
						u.Warnf("Could not evaluate? %v msg=%v", rhExpr.String(), msg.Body())
					}
				}
			}

		}
	}()
	wg.Wait()
	u.Info("leaving source scanner")
	i := uint64(0)
	for keyLeft, valLeft := range lh {
		if valRight, ok := rh[keyLeft]; ok {
			//u.Infof("found match?\n\t%d left=%v\n\t%d right=%v", len(valLeft), valLeft, len(valRight), valRight)
			msgs := mergeValuesMsgs(valLeft, valRight, m.leftStmt.Columns, m.rightStmt.Columns, nil)
			for _, msg := range msgs {
				u.Infof("msg:  %#v", msg)
				//outCh <- datasource.NewUrlValuesMsg(i, msg)
				msg.Id = i
				i++
				outCh <- msg
			}
		}
	}
	return nil
}

func joinValue(ctx *Context, node expr.Node, msg datasource.Message, cols map[string]*expr.Column) (string, bool) {

	if msg == nil {
		u.Warnf("got nil message?")
	}
	//u.Infof("got message: %T  %#v", msg, cols)
	switch mt := msg.(type) {
	case *datasource.SqlDriverMessage:
		msgReader := datasource.NewValueContextWrapper(mt, cols)
		joinVal, ok := vm.Eval(msgReader, node)
		//u.Debugf("msg: %#v", msgReader)
		//u.Infof("evaluating: ok?%v T:%T result=%v node '%v'", ok, joinVal, joinVal.ToString(), node.String())
		if !ok {
			u.Errorf("could not evaluate: %T %#v   %v", joinVal, joinVal, msg)
			return "", false
		}
		switch val := joinVal.(type) {
		case value.StringValue:
			return val.Val(), true
		default:
			u.Warnf("unknown type? %T", joinVal)
		}
	default:
		if msgReader, ok := msg.Body().(expr.ContextReader); ok {
			joinVal, ok := vm.Eval(msgReader, node)
			//u.Debugf("msg: %#v", msgReader)
			//u.Infof("evaluating: ok?%v T:%T result=%v node expr:%v", ok, joinVal, joinVal.ToString(), node.StringAST())
			if !ok {
				u.Errorf("could not evaluate: %v", msg)
				return "", false
			}
			switch val := joinVal.(type) {
			case value.StringValue:
				return val.Val(), true
			default:
				u.Warnf("unknown type? %T", joinVal)
			}
		} else {
			u.Errorf("could not convert to message reader: %T", msg.Body())
		}
	}

	return "", false
}

func mergeUvMsgs(lmsgs, rmsgs []datasource.Message, lcols, rcols map[string]*expr.Column) []*datasource.ContextUrlValues {
	out := make([]*datasource.ContextUrlValues, 0)
	for _, lm := range lmsgs {
		switch lmt := lm.Body().(type) {
		case *datasource.ContextUrlValues:
			for _, rm := range rmsgs {
				switch rmt := rm.Body().(type) {
				case *datasource.ContextUrlValues:
					// for k, val := range rmt.Data {
					// 	u.Debugf("k=%v v=%v", k, val)
					// }
					newMsg := datasource.NewContextUrlValues(url.Values{})
					newMsg = reAlias(newMsg, lmt.Data, lcols)
					newMsg = reAlias(newMsg, rmt.Data, rcols)
					//u.Debugf("pre:  %#v", lmt.Data)
					//u.Debugf("post:  %#v", newMsg.Data)
					out = append(out, newMsg)
				default:
					u.Warnf("uknown type: %T", rm)
				}
			}
		default:
			u.Warnf("uknown type: %T   %T", lmt, lm)
		}
	}
	return out
}

func mergeValuesMsgs(lmsgs, rmsgs []datasource.Message, lcols, rcols []*expr.Column, cols map[string]*expr.Column) []*datasource.SqlDriverMessageMap {
	out := make([]*datasource.SqlDriverMessageMap, 0)
	for _, lm := range lmsgs {
		switch lmt := lm.(type) {
		case *datasource.SqlDriverMessage:
			u.Warnf("got sql driver message: %#v", lmt.Vals)
			for _, rm := range rmsgs {
				switch rmt := rm.(type) {
				case *datasource.SqlDriverMessage:
					// for k, val := range rmt.Vals {
					// 	u.Debugf("k=%v v=%v", k, val)
					// }
					newMsg := datasource.NewSqlDriverMessageMap()
					newMsg = reAlias2(newMsg, lmt.Vals, lcols)
					newMsg = reAlias2(newMsg, rmt.Vals, rcols)
					//u.Debugf("pre:  %#v", lmt.Vals)
					u.Debugf("newMsg:  %#v", newMsg.Vals)
					out = append(out, newMsg)
				default:
					u.Warnf("unknown type: %T", rm)
				}
			}
		case *datasource.UrlValuesMsg:
			if uv, ok := lmt.Body().(*datasource.ContextUrlValues); ok {
				u.Warnf("got UrlValuesMsg message: %v   %#v", len(rmsgs), lmt.Body())
				for _, rm := range rmsgs {
					switch rmt := rm.Body().(type) {
					case *datasource.SqlDriverMessage:
						newMsg := datasource.NewSqlDriverMessageMap()
						newMsg = reAlias3(newMsg, uv, lcols, cols)
						newMsg = reAlias2(newMsg, rmt.Vals, rcols)
						//u.Debugf("pre:  %#v", lmt.Vals)
						u.Debugf("post:  %#v", newMsg.Vals)
						out = append(out, newMsg)
					case *datasource.ContextUrlValues:
						newMsg := mergeUv2(uv, rmt, cols)
						u.Debugf("post:  %#v", newMsg)
						out = append(out, newMsg)
					default:
						u.Warnf("unknown type: %T", rm)
					}
				}
			} else {
				u.Warnf("unknown type: %T   %#v", lmt.Body(), lmt.Body())
			}

		default:
			u.Warnf("unknown type: %T   %T", lmt, lm)
		}
	}
	return out
}

func mergeUv2(m1, m2 *datasource.ContextUrlValues, cols map[string]*expr.Column) *datasource.SqlDriverMessageMap {
	m3 := datasource.NewContextUrlValues(m1.Data)
	for k, val := range m2.Data {
		//u.Debugf("k=%v v=%v", k, val)
		m3.Data[k] = val
	}
	out := datasource.NewSqlDriverMessageMap()
	for k, vals := range m3.Data {
		if len(vals) > 0 {
			out.Vals[k] = vals[0]
		}
	}
	return out
}
func mergeUv(m1, m2 *datasource.ContextUrlValues) *datasource.ContextUrlValues {
	out := datasource.NewContextUrlValues(m1.Data)
	for k, val := range m2.Data {
		u.Debugf("k=%v v=%v", k, val)
		out.Data[k] = val
	}
	return out
}
func reAlias(m *datasource.ContextUrlValues, vals url.Values, cols map[string]*expr.Column) *datasource.ContextUrlValues {
	for k, val := range vals {
		if col, ok := cols[k]; !ok {
			//u.Warnf("Should not happen? missing %v  ", k)
		} else {
			//u.Infof("found: k=%v as=%v   val=%v", k, col.As, val)
			m.Data[col.As] = val
		}
	}
	return m
}
func reAlias2(m *datasource.SqlDriverMessageMap, vals []driver.Value, cols []*expr.Column) *datasource.SqlDriverMessageMap {
	for i, val := range vals {
		if i >= len(cols) {
			//u.Warnf("not enough cols? i=%v len(cols)=%v  %#v", i, len(cols), cols)
			continue
		}
		col := cols[i]
		u.Infof("found: i=%v as=%v   val=%v", i, col.As, val)
		m.Vals[col.As] = val
	}
	return m
}

func reAlias3(m *datasource.SqlDriverMessageMap, uv *datasource.ContextUrlValues, lcols []*expr.Column, cols map[string]*expr.Column) *datasource.SqlDriverMessageMap {
	for k, val := range uv.Data {
		if col, ok := cols[k]; ok {
			u.Infof("found: k=%v as=%v   val=%v", k, col.As, val)
			m.Vals[col.As] = val
		}
	}
	return m
}
