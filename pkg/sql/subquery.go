// Copyright 2015 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package sql

import (
	"github.com/cockroachdb/cockroach/pkg/sql/rowexec"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/errors"
)

// subquery represents a subquery expression in an expression tree
// after it has been converted to a query plan. It is stored in
// planTop.subqueryPlans.
type subquery struct {
	// subquery is used to show the subquery SQL when explaining plans.
	subquery tree.NodeFormatter
	execMode rowexec.SubqueryExecMode
	expanded bool
	started  bool
	plan     planMaybePhysical
	// rowCount is the estimated number of rows that plan will output, negative
	// if the stats weren't available to make a good estimate.
	rowCount int64
	result   tree.Datum
}

// EvalSubquery is called by `tree.Eval()` method implementations to
// retrieve the Datum result of a subquery.
func (p *planner) EvalSubquery(expr *tree.Subquery) (result tree.Datum, err error) {
	if expr.Idx == 0 {
		return nil, errors.AssertionFailedf("subquery %q was not processed", expr)
	}
	if expr.Idx < 0 || expr.Idx-1 >= len(p.curPlan.subqueryPlans) {
		return nil, errors.AssertionFailedf("subquery eval: invalid index %d for %q", expr.Idx, expr)
	}

	s := &p.curPlan.subqueryPlans[expr.Idx-1]
	if !s.started {
		return nil, errors.AssertionFailedf("subquery %d (%q) not started prior to evaluation", expr.Idx, expr)
	}
	return s.result, nil
}
