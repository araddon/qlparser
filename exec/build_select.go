package exec

import (
	"fmt"
	"strings"

	u "github.com/araddon/gou"

	"github.com/araddon/qlbridge/datasource"
	"github.com/araddon/qlbridge/expr"
)

func (m *JobBuilder) VisitSelect(stmt *expr.SqlSelect) (expr.Task, error) {

	u.Debugf("VisitSelect %+v", stmt)

	tasks := make(Tasks, 0)

	if len(stmt.From) == 0 {
		if stmt.SystemQry() {
			return m.VisitSelectSystemInfo(stmt)
		}
		u.Warnf("no from? %v", stmt.String())
		return nil, fmt.Errorf("No From for %v", stmt.String())

	} else if len(stmt.From) == 1 {

		stmt.From[0].Source = stmt
		task, err := m.VisitSubSelect(stmt.From[0])
		if err != nil {
			return nil, err
		}
		tasks.Add(task.(TaskRunner))

	} else {

		var prevTask TaskRunner
		var prevFrom *expr.SqlSource

		for i, from := range stmt.From {

			// Need to rewrite the From statement
			from.Rewrite(stmt)
			sourceTask, err := m.VisitSubSelect(from)
			if err != nil {
				u.Errorf("Could not visitsubselect %v  %s", err, from)
				return nil, err
			}

			// now fold into previous task
			curTask := sourceTask.(TaskRunner)
			if i != 0 {
				from.Seekable = true
				twoTasks := []TaskRunner{prevTask, curTask}
				curMergeTask := NewTaskParallel("select-sources", nil, twoTasks)
				tasks.Add(curMergeTask)

				// fold this source into previous
				in, err := NewJoinNaiveMerge(prevTask, curTask, prevFrom, from, m.Conf)
				if err != nil {
					return nil, err
				}
				tasks.Add(in)
			}
			prevTask = curTask
			prevFrom = from
			//u.Debugf("got task: %T", prevTask)
		}
	}

	if stmt.Where != nil {
		switch {
		case stmt.Where.Source != nil:
			u.Warnf("Found un-supported subquery: %#v", stmt.Where)
			return nil, fmt.Errorf("Unsupported Where Type")
		case stmt.Where.Expr != nil:
			//u.Debugf("adding where: %q", stmt.Where.Expr)
			where := NewWhereFinal(stmt.Where.Expr, stmt)
			tasks.Add(where)
		default:
			u.Warnf("Found un-supported where type: %#v", stmt.Where)
			return nil, fmt.Errorf("Unsupported Where Type")
		}

	}

	// Add a Projection to choose the columns for results
	projection := NewProjection(stmt)
	//u.Infof("adding projection: %#v", projection)
	tasks.Add(projection)

	return NewSequential("select", tasks), nil
}

// Build Column Name to Position index for given *source* (from) used to interpret
// positional []driver.Value args, mutate the *from* itself to hold this map
func buildColIndex(sourceConn datasource.SourceConn, from *expr.SqlSource) error {
	if from.Source == nil {
		return nil
	}
	colSchema, ok := sourceConn.(datasource.SchemaColumns)
	if !ok {
		u.Errorf("Could not create column Schema for %v  %T %#v", from.Name, sourceConn, sourceConn)
		return fmt.Errorf("Must Implement SchemaColumns for BuildColIndex")
	}
	from.BuildColIndex(colSchema.Columns())
	return nil
}

func (m *JobBuilder) VisitSubSelect(from *expr.SqlSource) (expr.Task, error) {

	if from.Source != nil {
		u.Debugf("VisitSubselect from.source = %q", from.Source)
	} else {
		u.Debugf("VisitSubselect from=%q", from)
	}

	tasks := make(Tasks, 0)
	needsJoinKey := false

	sourceFeatures := m.Conf.Sources.Get(from.SourceName())
	if sourceFeatures == nil {
		return nil, fmt.Errorf("Could not find source for %v", from.SourceName())
	}
	source, err := sourceFeatures.DataSource.Open(from.SourceName())
	if err != nil {
		return nil, err
	}

	sourcePlan, implementsSourceBuilder := source.(datasource.SourcePlanner)
	u.Debugf("source: tbl:%q  Builder?%v   %T  %#v", from.SourceName(), implementsSourceBuilder, source, source)
	// Must provider either Scanner, SourcePlanner, Seeker interfaces
	if implementsSourceBuilder {
		//  This is flawed, visitor pattern would have you pass in a object which implements interface
		//    but is one of many different objects that implement that interface so that the
		//    Accept() method calls the apppropriate method
		u.Warnf("yes, a SourcePlanner????")
		// builder := NewJobBuilder(conf, connInfo)
		// task, err := stmt.Accept(builder)
		builder, err := sourcePlan.Builder()
		if err != nil {
			u.Errorf("error on builder: %v", err)
			return nil, err
		} else if builder == nil {
			return nil, fmt.Errorf("No builder for %T", sourcePlan)
		}
		task, err := builder.VisitSubSelect(from)
		if err != nil {
			// PolyFill?
			return nil, err
		}
		if task != nil {
			return task, nil
		}
		u.Errorf("Could not source plan for %v  %T %#v", from.SourceName(), source, source)
	}
	scanner, hasScanner := source.(datasource.Scanner)
	if !hasScanner {
		u.Warnf("source %T does not implement datasource.Scanner", source)
		return nil, fmt.Errorf("%T Must Implement Scanner for %q", source, from.String())
	}

	switch {

	case from.Source != nil && len(from.JoinNodes()) > 0:
		// This is a source that is part of a join expression
		if err := buildColIndex(scanner, from); err != nil {
			return nil, err
		}
		sourceTask := NewSourceJoin(from, scanner)
		tasks.Add(sourceTask)
		needsJoinKey = true

	default:
		// If we have table name and no Source(sub-query/join-query) then just read source
		if err := buildColIndex(scanner, from); err != nil {
			return nil, err
		}
		sourceTask := NewSource(from, scanner)
		tasks.Add(sourceTask)

	}

	if from.Source != nil && from.Source.Where != nil {
		switch {
		case from.Source.Where.Expr != nil:
			//u.Debugf("adding where: %q", from.Source.Where.Expr)
			where := NewWhereFilter(from.Source.Where.Expr, from.Source)
			tasks.Add(where)
		default:
			u.Warnf("Found un-supported where type: %#v", from.Source)
			return nil, fmt.Errorf("Unsupported Where clause:  %q", from)
		}
	}

	if needsJoinKey {
		joinKeyTask, err := NewJoinKey(from, m.Conf)
		if err != nil {
			return nil, err
		}
		tasks.Add(joinKeyTask)
	}
	// Plan?   Parallel?  hash?
	return NewSequential("sub-select", tasks), nil
}

// queries for internal schema/variables such as:
//
//    select @@max_allowed_packets
//    select current_user()
//    select connection_id()
//    select timediff(curtime(), utc_time())
//
func (m *JobBuilder) VisitSelectSystemInfo(stmt *expr.SqlSelect) (expr.Task, error) {

	u.Debugf("VisitSelectSchemaInfo %+v", stmt)
	tasks := make(Tasks, 0)

	task, err := m.VisitSubSelect(stmt.From[0])
	if err != nil {
		return nil, err
	}
	tasks.Add(task.(TaskRunner))

	if stmt.Where != nil {
		switch {
		case stmt.Where.Source != nil:
			u.Warnf("Found un-supported subquery: %#v", stmt.Where)
			return nil, fmt.Errorf("Unsupported Where Type")
		case stmt.Where.Expr != nil:
			//u.Debugf("adding where: %q", stmt.Where.Expr)
			where := NewWhereFinal(stmt.Where.Expr, stmt)
			tasks.Add(where)
		default:
			u.Warnf("Found un-supported where type: %#v", stmt.Where)
			return nil, fmt.Errorf("Unsupported Where Type")
		}

	}

	// Add a Projection to choose the columns for results
	projection := NewProjection(stmt)
	//u.Infof("adding projection: %#v", projection)
	tasks.Add(projection)

	return NewSequential("select-schemainfo", tasks), nil
}

func createProjection(sqlJob *SqlJob, stmt *expr.SqlSelect) error {

	if sqlJob.Projection != nil {
		u.Warnf("allready has projection? %#v", sqlJob)
		return nil
	}
	//u.Debugf("createProjection %s", stmt.String())
	p := expr.NewProjection()
	for _, from := range stmt.From {
		//u.Infof("info: %#v", from)
		fromName := strings.ToLower(from.SourceName())
		tbl, err := sqlJob.Conf.Table(fromName)
		if err != nil {
			u.Errorf("could not get table: %v", err)
			return err
		} else if tbl == nil {
			u.Errorf("no table? %v", from.Name)
			return fmt.Errorf("Table not found %q", from.Name)
		} else {
			//u.Infof("getting cols? %v", len(from.Columns))
			cols := from.UnAliasedColumns()
			if len(cols) == 0 && len(stmt.From) == 1 {
				//from.Columns = stmt.Columns
				u.Warnf("no cols?")
			}
			for _, col := range cols {
				if schemaCol, ok := tbl.FieldMap[col.SourceField]; ok {
					u.Infof("adding projection col: %v %v", col.As, schemaCol.Type.String())
					p.AddColumnShort(col.As, schemaCol.Type)
				} else {
					u.Errorf("schema col not found:  vals=%#v", col)
				}
			}
		}
	}
	return nil
}
