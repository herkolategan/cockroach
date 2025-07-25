// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package memo

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/cat"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/props/physical"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/catid"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/idxtype"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree/treewindow"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/treeprinter"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// ScalarFmtInterceptor is a callback that can be set to a custom formatting
// function. If the function returns a non-empty string, the normal formatting
// code is bypassed.
var ScalarFmtInterceptor func(f *ExprFmtCtx, expr opt.ScalarExpr) string

// ExprFmtFlags controls which properties of the expression are shown in
// formatted output.
type ExprFmtFlags int

// ExprFmtFlags is a bitmask that can contain any of the flags below. The zero
// value represents showing all possible information when formatting an
// expression. Flags are used to hide information. For example, to hide
// constraints and functional dependencies, you can combine the flags
// ExprFmtHideConstraints and ExprFmtHideFuncDeps:
//
//	myFlag := ExprFmtHideConstraints|ExprFmtHideFuncDeps
//
// New flags should be added before the ExprFmtHideAll flag.
const (
	// ExprFmtShowAll shows all properties of the expression.
	ExprFmtShowAll ExprFmtFlags = 0

	// ExprFmtHideMiscProps does not show outer columns, row cardinality, provided
	// orderings, side effects, or error text in the output.
	ExprFmtHideMiscProps ExprFmtFlags = 1 << (iota - 1)

	// ExprFmtHideConstraints does not show inferred constraints in the output.
	ExprFmtHideConstraints

	// ExprFmtHideFuncDeps does not show functional dependencies in the output.
	ExprFmtHideFuncDeps

	// ExprFmtHideRuleProps does not show rule-specific properties in the output.
	ExprFmtHideRuleProps

	// ExprFmtHideStats does not show statistics in the output.
	ExprFmtHideStats

	// ExprFmtHideHistograms does not show statistics histograms in the output.
	// Note that if ExprFmtHideStats is set, histograms are never included
	// in the output.
	ExprFmtHideHistograms

	// ExprFmtHideCost does not show expression cost in the output.
	ExprFmtHideCost

	// ExprFmtHideQualifications removes the qualification from column labels
	// (except when a shortened name would be ambiguous).
	ExprFmtHideQualifications

	// ExprFmtHideScalars removes subtrees that contain only scalars and replaces
	// them with the SQL expression (if possible).
	ExprFmtHideScalars

	// ExprFmtHidePhysProps hides all required physical properties, except for
	// Presentation (see ExprFmtHideColumns).
	ExprFmtHidePhysProps

	// ExprFmtHideTypes hides type information from columns and scalar
	// expressions.
	ExprFmtHideTypes

	// ExprFmtHideNotNull hides the !null specifier from columns.
	ExprFmtHideNotNull

	// ExprFmtHideColumns removes column information.
	ExprFmtHideColumns

	// ExprFmtHideNotVisibleIndexInfo hides information about invisible index. We
	// will only show invisible index info when we are actually scanning an
	// invisible index. If this flag is off, we will always show invisible index
	// info regardless of whether we are actually scanning an invisible index.
	ExprFmtHideNotVisibleIndexInfo

	// ExprFmtHideFastPathChecks hides information about insert fast path unique
	// check expressions. These expressions are not executed, but used to find
	// constrained index scans which may be used to perform the check as a
	// fast path check. Most of the time these expressions should not be displayed
	// due to the fact that they are not actually executed.
	ExprFmtHideFastPathChecks

	// ExprFmtHideAll shows only the basic structure of the expression.
	// Note: this flag should be used judiciously, as its meaning changes whenever
	// we add more flags.
	ExprFmtHideAll ExprFmtFlags = (1 << iota) - 1
)

// HasFlags tests whether the given flags are all set.
func (f ExprFmtFlags) HasFlags(subset ExprFmtFlags) bool {
	return f&subset == subset
}

// FormatExpr returns a string representation of the given expression, formatted
// according to the specified flags.
func FormatExpr(
	ctx context.Context,
	e opt.Expr,
	flags ExprFmtFlags,
	redactableValues bool,
	mem *Memo,
	catalog cat.Catalog,
) string {
	if catalog == nil {
		// Automatically hide qualifications if we have no catalog.
		flags |= ExprFmtHideQualifications
	}
	f := MakeExprFmtCtx(ctx, flags, redactableValues, mem, catalog)
	f.FormatExpr(e)
	return f.Buffer.String()
}

// ExprFmtCtx is passed as context to expression formatting functions, which
// need to know the formatting flags and memo in order to format. In addition,
// a reusable bytes buffer avoids unnecessary allocations.
type ExprFmtCtx struct {
	Ctx    context.Context
	Buffer *bytes.Buffer

	// Flags controls which information is included in the formatted expression.
	Flags ExprFmtFlags

	// If RedactableValues is true we surround any constant values or other user
	// data from the formatted expression with redaction markers, including spans.
	RedactableValues bool

	// Memo must contain any expression that is formatted. It is not used for
	// scalar expressions. If it is nil for relational expressions, formatting may
	// result in a nil-pointer panic.
	Memo *Memo

	// Catalog must be set unless the ExprFmtHideQualifications flag is set.
	Catalog cat.Catalog

	// nameGen is used to generate a unique name for each relational
	// subexpression when Memo.saveTablesPrefix is non-empty. These names
	// correspond to the tables that would be saved if the query were run
	// with the session variable `save_tables_prefix` set to the same value.
	nameGen *ExprNameGenerator

	// seenUDFs is used to ensure that formatting of recursive UDFs does not
	// infinitely recurse.
	seenUDFs map[*UDFDefinition]struct{}

	// tailCalls allows for quick lookup of all the routines in tail-call position
	// when the last body statement of a routine is formatted.
	tailCalls map[opt.ScalarExpr]struct{}
}

// makeExprFmtCtxForString creates an expression formatting context from a new
// buffer with the context.Background(). This method is designed to be used in
// stringer.String implementations.
func makeExprFmtCtxForString(flags ExprFmtFlags, mem *Memo, catalog cat.Catalog) ExprFmtCtx {
	return MakeExprFmtCtxBuffer(
		context.Background(), &bytes.Buffer{}, flags, false /* redactableValues */, mem, catalog,
	)
}

// MakeExprFmtCtx creates an expression formatting context from a new buffer.
func MakeExprFmtCtx(
	ctx context.Context, flags ExprFmtFlags, redactableValues bool, mem *Memo, catalog cat.Catalog,
) ExprFmtCtx {
	return MakeExprFmtCtxBuffer(ctx, &bytes.Buffer{}, flags, redactableValues, mem, catalog)
}

// MakeExprFmtCtxBuffer creates an expression formatting context from an
// existing buffer.
func MakeExprFmtCtxBuffer(
	ctx context.Context,
	buf *bytes.Buffer,
	flags ExprFmtFlags,
	redactableValues bool,
	mem *Memo,
	catalog cat.Catalog,
) ExprFmtCtx {
	var nameGen *ExprNameGenerator
	if mem != nil && mem.saveTablesPrefix != "" {
		nameGen = NewExprNameGenerator(mem.saveTablesPrefix)
	}
	return ExprFmtCtx{
		Ctx:              ctx,
		Buffer:           buf,
		Flags:            flags,
		RedactableValues: redactableValues,
		Memo:             mem,
		Catalog:          catalog,
		nameGen:          nameGen,
		seenUDFs:         make(map[*UDFDefinition]struct{}),
	}
}

// HasFlags tests whether the given flags are all set.
func (f *ExprFmtCtx) HasFlags(subset ExprFmtFlags) bool {
	return f.Flags.HasFlags(subset)
}

// FormatExpr constructs a treeprinter view of the given expression for testing
// and debugging, according to the flags in this context.
func (f *ExprFmtCtx) FormatExpr(e opt.Expr) {
	tp := treeprinter.New()
	f.formatExpr(e, tp)
	f.Buffer.Reset()
	f.Buffer.WriteString(tp.String())
}

func (f *ExprFmtCtx) space() {
	f.Buffer.WriteByte(' ')
}

func (f *ExprFmtCtx) formatExpr(e opt.Expr, tp treeprinter.Node) {
	scalar, ok := e.(opt.ScalarExpr)
	if ok {
		f.formatScalar(scalar, tp)
	} else {
		f.formatRelational(e.(RelExpr), tp)
	}
}

func (f *ExprFmtCtx) formatRelational(e RelExpr, tp treeprinter.Node) {
	md := f.Memo.Metadata()
	required := e.RequiredPhysical()
	relational := e.Relational()
	if required == nil {
		// required can be nil before optimization has taken place.
		required = physical.MinRequired
	}

	// Special cases for merge-join and lookup-join: we want the type of the join
	// to show up first.
	f.Buffer.Reset()
	switch t := e.(type) {
	case *MergeJoinExpr:
		fmt.Fprintf(f.Buffer, "%v (merge)", t.JoinType)

	case *LookupJoinExpr:
		fmt.Fprintf(f.Buffer, "%v (lookup", t.JoinType)
		FormatPrivate(f, e.Private(), required)
		f.Buffer.WriteByte(')')

	case *InvertedJoinExpr:
		fmt.Fprintf(f.Buffer, "%v (inverted", t.JoinType)
		FormatPrivate(f, e.Private(), required)
		f.Buffer.WriteByte(')')

	case *ZigzagJoinExpr:
		fmt.Fprintf(f.Buffer, "%v (zigzag", opt.InnerJoinOp)
		FormatPrivate(f, e.Private(), required)
		f.Buffer.WriteByte(')')

	case *ScanExpr, *PlaceholderScanExpr, *IndexJoinExpr, *ShowTraceForSessionExpr,
		*InsertExpr, *UpdateExpr, *UpsertExpr, *DeleteExpr, *LockExpr, *SequenceSelectExpr,
		*WindowExpr, *VectorSearchExpr, *VectorMutationSearchExpr, *OpaqueRelExpr,
		*OpaqueMutationExpr, *OpaqueDDLExpr, *AlterTableSplitExpr, *AlterTableUnsplitExpr,
		*AlterTableUnsplitAllExpr, *AlterTableRelocateExpr, *AlterRangeRelocateExpr,
		*ControlJobsExpr, *CancelQueriesExpr, *CancelSessionsExpr, *CreateViewExpr,
		*ExportExpr, *ShowCompletionsExpr:
		fmt.Fprintf(f.Buffer, "%v", e.Op())
		FormatPrivate(f, e.Private(), required)

	case *GroupByExpr:
		fmt.Fprintf(f.Buffer, "%v ", e.Op())
		groupingColOrderType := e.Private().(*GroupingPrivate).GroupingOrderType(&required.Ordering)
		if groupingColOrderType == Streaming {
			fmt.Fprintf(f.Buffer, "(streaming)")
		} else if groupingColOrderType == PartialStreaming {
			fmt.Fprintf(f.Buffer, "(partial streaming)")
		} else {
			fmt.Fprintf(f.Buffer, "(hash)")
		}

	case *SortExpr:
		if t.InputOrdering.Any() {
			fmt.Fprintf(f.Buffer, "%v", e.Op())
		} else {
			fmt.Fprintf(f.Buffer, "%v (segmented)", e.Op())
		}

	case *WithExpr:
		fmt.Fprintf(f.Buffer, "%v &%d", e.Op(), t.ID)
		if t.Name != "" {
			fmt.Fprintf(f.Buffer, " (%s)", t.Name)
		}

	case *WithScanExpr:
		fmt.Fprintf(f.Buffer, "%v &%d", e.Op(), t.With)
		if t.Name != "" {
			fmt.Fprintf(f.Buffer, " (%s)", t.Name)
		}

	default:
		fmt.Fprintf(f.Buffer, "%v", e.Op())
		if opt.IsJoinNonApplyOp(t) {
			// All join ops that weren't handled above execute as a hash join.
			if leftEqCols := ExtractJoinEqualityLeftColumns(
				e.Child(0).(RelExpr).Relational().OutputCols,
				e.Child(1).(RelExpr).Relational().OutputCols,
				*e.Child(2).(*FiltersExpr),
			); len(leftEqCols) == 0 {
				// The case where there are no equality columns is executed as a
				// degenerate case of hash join; let's be explicit about that.
				f.Buffer.WriteString(" (cross)")
			} else {
				f.Buffer.WriteString(" (hash)")
			}
		}
	}

	tp = tp.Child(f.Buffer.String())

	if f.nameGen != nil {
		name := f.nameGen.GenerateName(e.Op())
		tp.Childf("save-table-name: %s", name)
	}

	var colList opt.ColList
	// Special handling to improve the columns display for certain ops.
	switch t := e.(type) {
	case *ProjectExpr:
		// We want the synthesized column IDs to map 1-to-1 to the projections,
		// and the pass-through columns at the end.

		// Get the list of columns from the ProjectionsOp, which has the natural
		// order.
		for i := range t.Projections {
			colList = append(colList, t.Projections[i].Col)
		}

		// Add pass-through columns.
		t.Passthrough.ForEach(func(i opt.ColumnID) {
			colList = append(colList, i)
		})

	case *ValuesExpr:
		colList = t.Cols

	case *LiteralValuesExpr:
		colList = t.Cols

	case *UnionExpr, *IntersectExpr, *ExceptExpr,
		*UnionAllExpr, *IntersectAllExpr, *ExceptAllExpr, *LocalityOptimizedSearchExpr:
		colList = e.Private().(*SetPrivate).OutCols

	default:
		// Fall back to writing output columns in column id order.
		colList = e.Relational().OutputCols.ToList()
	}

	f.formatColumns(e, tp, colList, required.Presentation)

	switch t := e.(type) {
	// Special-case handling for GroupBy private; print grouping columns
	// and internal ordering in addition to full set of columns.
	case *GroupByExpr, *ScalarGroupByExpr, *DistinctOnExpr, *EnsureDistinctOnExpr,
		*UpsertDistinctOnExpr, *EnsureUpsertDistinctOnExpr:
		private := e.Private().(*GroupingPrivate)
		if !f.HasFlags(ExprFmtHideColumns) && !private.GroupingCols.Empty() {
			f.formatRelColList(e, tp, "grouping columns:", private.GroupingCols.ToList())
		}
		if !f.HasFlags(ExprFmtHidePhysProps) && !private.Ordering.Any() {
			tp.Childf("internal-ordering: %s", private.Ordering)
		}
		if !f.HasFlags(ExprFmtHideMiscProps) && private.ErrorOnDup != "" {
			tp.Childf("error: \"%s\"", private.ErrorOnDup)
		}

	case *TopKExpr:
		if !f.HasFlags(ExprFmtHidePhysProps) && !t.Ordering.Any() {
			tp.Childf("internal-ordering: %s", t.Ordering)
		}
		tp.Childf("k: %d", t.K)

	case *LimitExpr:
		if !f.HasFlags(ExprFmtHidePhysProps) && !t.Ordering.Any() {
			tp.Childf("internal-ordering: %s", t.Ordering)
		}

	case *OffsetExpr:
		if !f.HasFlags(ExprFmtHidePhysProps) && !t.Ordering.Any() {
			tp.Childf("internal-ordering: %s", t.Ordering)
		}

	case *Max1RowExpr:
		if !f.HasFlags(ExprFmtHideMiscProps) {
			tp.Childf("error: \"%s\"", t.ErrorText)
		}

	// Special-case handling for set operators to show the left and right
	// input columns that correspond to the output columns.
	case *UnionExpr, *IntersectExpr, *ExceptExpr,
		*UnionAllExpr, *IntersectAllExpr, *ExceptAllExpr, *LocalityOptimizedSearchExpr:
		private := e.Private().(*SetPrivate)
		if !f.HasFlags(ExprFmtHideColumns) {
			f.formatRelColList(e, tp, "left columns:", private.LeftCols)
			f.formatRelColList(e, tp, "right columns:", private.RightCols)
		}
		if !f.HasFlags(ExprFmtHidePhysProps) && !private.Ordering.Any() {
			tp.Childf("internal-ordering: %s", private.Ordering)
		}

	case *ScanExpr, *PlaceholderScanExpr:
		private := t.Private().(*ScanPrivate)
		if t.Op() == opt.ScanOp && private.IsCanonical() {
			// For the canonical scan, show the expressions attached to the TableMeta.
			tab := md.TableMeta(private.Table)
			if tab.Constraints != nil {
				c := tp.Childf("check constraint expressions")
				for i := 0; i < tab.Constraints.ChildCount(); i++ {
					f.formatExpr(tab.Constraints.Child(i), c)
				}
			}
			if len(tab.ComputedCols) > 0 {
				c := tp.Childf("computed column expressions")
				cols := make(opt.ColList, 0, len(tab.ComputedCols))
				for col := range tab.ComputedCols {
					cols = append(cols, col)
				}
				sort.Slice(cols, func(i, j int) bool {
					return cols[i] < cols[j]
				})
				for _, col := range cols {
					f.Buffer.Reset()
					f.formatExpr(tab.ComputedCols[col], c.Child(f.ColumnString(col)))
				}
			}
			partialIndexPredicates := tab.PartialIndexPredicatesUnsafe()
			if partialIndexPredicates != nil {
				c := tp.Child("partial index predicates")
				indexOrds := make(cat.IndexOrdinals, 0, len(partialIndexPredicates))
				for ord := range partialIndexPredicates {
					indexOrds = append(indexOrds, ord)
				}
				sort.Ints(indexOrds)
				for _, ord := range indexOrds {
					name := string(tab.Table.Index(ord).Name())
					f.Buffer.Reset()
					f.formatScalarWithLabel(name, partialIndexPredicates[ord], c)
				}
			}
		}
		if c := private.Constraint; c != nil {
			if c.IsContradiction() {
				tp.Childf("constraint: contradiction")
			} else if c.Spans.Count() == 1 {
				tp.Childf(
					"constraint: %s: %s", c.Columns.String(),
					cat.MaybeMarkRedactable(c.Spans.Get(0).String(), f.RedactableValues),
				)
			} else {
				n := tp.Childf("constraint: %s", c.Columns.String())
				for i := 0; i < c.Spans.Count(); i++ {
					n.Child(cat.MaybeMarkRedactable(c.Spans.Get(i).String(), f.RedactableValues))
				}
			}
		}
		if ic := private.InvertedConstraint; ic != nil {
			idx := md.Table(private.Table).Index(private.Index)
			var b strings.Builder
			for i := idx.PrefixColumnCount(); i < idx.KeyColumnCount(); i++ {
				b.WriteRune('/')
				b.WriteString(fmt.Sprintf("%d", private.Table.ColumnID(idx.Column(i).Ordinal())))
			}
			n := tp.Childf("inverted constraint: %s", b.String())
			ic.Format(n, "spans", f.RedactableValues)
		}
		if private.HardLimit.IsSet() {
			tp.Childf("limit: %s", private.HardLimit)
		}

		if private.shouldPrintFlags(md, f.HasFlags(ExprFmtHideNotVisibleIndexInfo)) {
			var b strings.Builder
			b.WriteString("flags:")
			if private.Flags.NoIndexJoin {
				b.WriteString(" no-index-join")
			}
			if private.Flags.ForceIndex {
				idx := md.Table(private.Table).Index(private.Flags.Index)
				dir := ""
				switch private.Flags.Direction {
				case tree.DefaultDirection:
				case tree.Ascending:
					dir = ",fwd"
				case tree.Descending:
					dir = ",rev"
				}
				b.WriteString(fmt.Sprintf(" force-index=%s%s", idx.Name(), dir))
			}
			if private.Flags.NoZigzagJoin {
				b.WriteString(" no-zigzag-join")
			}
			if private.Flags.NoFullScan {
				b.WriteString(" no-full-scan")
			}
			if private.Flags.AvoidFullScan {
				b.WriteString(" avoid-full-scan")
			}
			if private.Flags.ForceZigzag {
				if private.Flags.ZigzagIndexes.Empty() {
					b.WriteString(" force-zigzag")
				} else {
					b.WriteString(" force-zigzag=")
					s := private.Flags.ZigzagIndexes
					needComma := false
					for i, ok := s.Next(0); ok; i, ok = s.Next(i + 1) {
						idx := md.Table(private.Table).Index(i)
						if needComma {
							b.WriteByte(',')
						}
						b.WriteString(string(idx.Name()))
						needComma = true
					}
				}
			}

			if private.Flags.DisableNotVisibleIndex && private.showNotVisibleIndexInfo(md, f.HasFlags(ExprFmtHideNotVisibleIndexInfo)) {
				b.WriteString(" disabled not visible index feature")
			}
			tp.Child(b.String())
		}
		f.formatLocking(tp, private.Locking)

	case *InvertedFilterExpr:
		var b strings.Builder
		b.WriteRune('/')
		b.WriteString(fmt.Sprintf("%d", t.InvertedColumn))
		n := tp.Childf("inverted expression: %s", b.String())
		t.InvertedExpression.Format(n, false /* includeSpansToRead */, f.RedactableValues)
		if t.PreFiltererState != nil {
			n := tp.Childf("pre-filterer expression")
			f.formatExpr(t.PreFiltererState.Expr, n)
		}

	case *IndexJoinExpr:
		f.formatLocking(tp, t.Locking)

	case *LookupJoinExpr:
		if !t.Flags.Empty() {
			tp.Childf("flags: %s", t.Flags.String())
		}
		if !f.HasFlags(ExprFmtHideColumns) {
			if len(t.KeyCols) > 0 {
				idxCols := make(opt.ColList, len(t.KeyCols))
				idx := md.Table(t.Table).Index(t.Index)
				for i := range idxCols {
					idxCols[i] = t.Table.ColumnID(idx.Column(i).Ordinal())
				}
				tp.Childf("key columns: %v = %v", t.KeyCols, idxCols)
			}
			if len(t.LookupExpr) > 0 {
				n := tp.Childf("lookup expression")
				f.formatExpr(&t.LookupExpr, n)
			}
			if len(t.RemoteLookupExpr) > 0 {
				n := tp.Childf("remote lookup expression")
				f.formatExpr(&t.RemoteLookupExpr, n)
			}
		}
		if t.LookupColsAreTableKey {
			tp.Childf("lookup columns are key")
		}
		if t.IsFirstJoinInPairedJoiner {
			f.formatRelColList(e, tp, "first join in paired joiner; continuation column:", opt.ColList{t.ContinuationCol})
		}
		if t.IsSecondJoinInPairedJoiner {
			tp.Childf("second join in paired joiner")
		}
		f.formatLocking(tp, t.Locking)

	case *InvertedJoinExpr:
		if !t.Flags.Empty() {
			tp.Childf("flags: %s", t.Flags.String())
		}
		if !f.HasFlags(ExprFmtHideColumns) && len(t.PrefixKeyCols) > 0 {
			idxCols := make(opt.ColList, len(t.PrefixKeyCols))
			idx := md.Table(t.Table).Index(t.Index)
			for i := range idxCols {
				idxCols[i] = t.Table.ColumnID(idx.Column(i).Ordinal())
			}
			tp.Childf("prefix key columns: %v = %v", t.PrefixKeyCols, idxCols)
		}
		if t.IsFirstJoinInPairedJoiner {
			f.formatRelColList(e, tp, "first join in paired joiner; continuation column:", opt.ColList{t.ContinuationCol})
		}
		n := tp.Child("inverted-expr")
		f.formatExpr(t.InvertedExpr, n)
		f.formatLocking(tp, t.Locking)

	case *ZigzagJoinExpr:
		if !f.HasFlags(ExprFmtHideColumns) {
			tp.Childf("eq columns: %v = %v", t.LeftEqCols, t.RightEqCols)
			leftVals := make([]tree.Datum, len(t.LeftFixedCols))
			rightVals := make([]tree.Datum, len(t.RightFixedCols))
			// FixedVals is always going to be a ScalarListExpr, containing tuples,
			// containing one ScalarListExpr, containing ConstExprs.
			for i := range t.LeftFixedCols {
				leftVals[i] = ExtractConstDatum(t.FixedVals[0].Child(0).Child(i))
			}
			for i := range t.RightFixedCols {
				rightVals[i] = ExtractConstDatum(t.FixedVals[1].Child(0).Child(i))
			}
			tp.Childf("left fixed columns: %v = %v", t.LeftFixedCols, leftVals)
			tp.Childf("right fixed columns: %v = %v", t.RightFixedCols, rightVals)
		}
		f.formatLockingWithPrefix(tp, "left ", t.LeftLocking)
		f.formatLockingWithPrefix(tp, "right ", t.RightLocking)

	case *MergeJoinExpr:
		if !t.Flags.Empty() {
			tp.Childf("flags: %s", t.Flags.String())
		}
		if !f.HasFlags(ExprFmtHidePhysProps) {
			tp.Childf("left ordering: %s", t.LeftEq)
			tp.Childf("right ordering: %s", t.RightEq)
		}

	case *InsertExpr:
		f.formatIndexes(tp, "arbiter", t.ArbiterIndexes, t.Table)
		f.formatArbiterConstraints(tp, t.ArbiterConstraints, t.Table)
		f.formatIndexes(tp, "unique w/tombstone", t.UniqueWithTombstoneIndexes, t.Table)
		if !f.HasFlags(ExprFmtHideColumns) {
			if len(colList) == 0 {
				tp.Child("columns: <none>")
			}
			f.formatMutationCols(e, tp, "insert-mapping:", t.InsertCols, t.Table)
			f.formatMutationCols(e, tp, "return-mapping:", t.ReturnCols, t.Table)
			f.formatOptionalColList(e, tp, "check columns:", t.CheckCols)
			f.formatOptionalColList(e, tp, "partial index put columns:", t.PartialIndexPutCols)
			f.formatOptionalColList(e, tp, "vector index put partition columns:", t.VectorIndexPutPartitionCols)
			f.formatOptionalColList(e, tp, "vector index put quantized vector columns:", t.VectorIndexPutQuantizedVecCols)
			f.formatBeforeTriggers(tp, t.Table, tree.TriggerEventInsert)
			f.formatMutationCommon(tp, &t.MutationPrivate)
		}

	case *UpdateExpr:
		f.formatIndexes(tp, "unique w/tombstone", t.UniqueWithTombstoneIndexes, t.Table)
		if !f.HasFlags(ExprFmtHideColumns) {
			if len(colList) == 0 {
				tp.Child("columns: <none>")
			}
			f.formatOptionalColList(e, tp, "fetch columns:", t.FetchCols)
			f.formatOptionalColList(e, tp, "passthrough columns:", opt.OptionalColList(t.PassthroughCols))
			f.formatMutationCols(e, tp, "update-mapping:", t.UpdateCols, t.Table)
			f.formatMutationCols(e, tp, "return-mapping:", t.ReturnCols, t.Table)
			f.formatOptionalColList(e, tp, "check columns:", t.CheckCols)
			f.formatOptionalColList(e, tp, "partial index put columns:", t.PartialIndexPutCols)
			f.formatOptionalColList(e, tp, "partial index del columns:", t.PartialIndexDelCols)
			f.formatOptionalColList(e, tp, "vector index del partition columns:", t.VectorIndexDelPartitionCols)
			f.formatOptionalColList(e, tp, "vector index put partition columns:", t.VectorIndexPutPartitionCols)
			f.formatOptionalColList(e, tp, "vector index put quantized vector columns:", t.VectorIndexPutQuantizedVecCols)
			f.formatBeforeTriggers(tp, t.Table, tree.TriggerEventUpdate)
			f.formatMutationCommon(tp, &t.MutationPrivate)
		}

	case *UpsertExpr:
		f.formatIndexes(tp, "arbiter", t.ArbiterIndexes, t.Table)
		f.formatArbiterConstraints(tp, t.ArbiterConstraints, t.Table)
		f.formatIndexes(tp, "unique w/tombstone", t.UniqueWithTombstoneIndexes, t.Table)
		if !f.HasFlags(ExprFmtHideColumns) {
			if len(colList) == 0 {
				tp.Child("columns: <none>")
			}
			if t.CanaryCol != 0 {
				f.formatRelColList(e, tp, "canary column:", opt.ColList{t.CanaryCol})
				f.formatOptionalColList(e, tp, "fetch columns:", t.FetchCols)
				f.formatMutationCols(e, tp, "insert-mapping:", t.InsertCols, t.Table)
				f.formatMutationCols(e, tp, "update-mapping:", t.UpdateCols, t.Table)
				f.formatMutationCols(e, tp, "return-mapping:", t.ReturnCols, t.Table)
			} else {
				f.formatMutationCols(e, tp, "upsert-mapping:", t.InsertCols, t.Table)
			}
			f.formatOptionalColList(e, tp, "check columns:", t.CheckCols)
			f.formatOptionalColList(e, tp, "partial index put columns:", t.PartialIndexPutCols)
			f.formatOptionalColList(e, tp, "partial index del columns:", t.PartialIndexDelCols)
			f.formatOptionalColList(e, tp, "vector index del partition columns:", t.VectorIndexDelPartitionCols)
			f.formatOptionalColList(e, tp, "vector index put partition columns:", t.VectorIndexPutPartitionCols)
			f.formatOptionalColList(e, tp, "vector index put quantized vector columns:", t.VectorIndexPutQuantizedVecCols)
			f.formatBeforeTriggers(tp, t.Table, tree.TriggerEventInsert, tree.TriggerEventUpdate)
			f.formatMutationCommon(tp, &t.MutationPrivate)
		}

	case *DeleteExpr:
		if !f.HasFlags(ExprFmtHideColumns) {
			if len(colList) == 0 {
				tp.Child("columns: <none>")
			}
			f.formatOptionalColList(e, tp, "fetch columns:", t.FetchCols)
			f.formatMutationCols(e, tp, "return-mapping:", t.ReturnCols, t.Table)
			f.formatOptionalColList(e, tp, "passthrough columns", opt.OptionalColList(t.PassthroughCols))
			f.formatOptionalColList(e, tp, "partial index del columns:", t.PartialIndexDelCols)
			f.formatOptionalColList(e, tp, "vector index del partition columns:", t.VectorIndexDelPartitionCols)
			f.formatBeforeTriggers(tp, t.Table, tree.TriggerEventDelete)
			f.formatMutationCommon(tp, &t.MutationPrivate)
		}

	case *LockExpr:
		f.formatColList(tp, "key columns:", t.KeyCols, opt.ColSet{} /* notNullCols */)
		tp.Childf("lock columns: %v", t.LockCols)
		f.formatLocking(tp, t.Locking)

	case *WithExpr:
		switch t.Mtr {
		case tree.CTEMaterializeAlways:
			tp.Child("materialized")
		case tree.CTEMaterializeNever:
			tp.Child("not-materialized")
		}

	case *WithScanExpr:
		if !f.HasFlags(ExprFmtHideColumns) {
			child := tp.Child("mapping:")
			for i := range t.InCols {
				f.Buffer.Reset()
				f.space()
				f.formatCol("" /* label */, t.InCols[i], opt.ColSet{} /* notNullCols */)
				f.Buffer.WriteString(" => ")
				f.formatCol("" /* label */, t.OutCols[i], opt.ColSet{} /* notNullCols */)
				child.Child(f.Buffer.String())
			}
		}

	case *VectorSearchExpr:
		tp.Childf("target nearest neighbors: %d", t.TargetNeighborCount)
		if t.PrefixConstraint != nil {
			n := tp.Childf("prefix constraint: %s", t.PrefixConstraint.Columns.String())
			for i := 0; i < t.PrefixConstraint.Spans.Count(); i++ {
				spanString := t.PrefixConstraint.Spans.Get(i).String()
				n.Child(cat.MaybeMarkRedactable(spanString, f.RedactableValues))
			}
		}

	case *VectorMutationSearchExpr:
		if t.IsIndexPut {
			tp.Childf("index put")
		} else {
			tp.Childf("index del")
		}
		if len(t.PrefixKeyCols) > 0 {
			tp.Childf("prefix key columns: %v", t.PrefixKeyCols)
		}
		tp.Childf("query vector column: %s", f.ColumnString(t.QueryVectorCol))
		if len(t.SuffixKeyCols) > 0 {
			tp.Childf("suffix key columns: %v", t.SuffixKeyCols)
		}
		tp.Childf("partition col: %s", f.ColumnString(t.PartitionCol))
		if t.QuantizedVectorCol != 0 {
			tp.Childf("quantized vector col: %s", f.ColumnString(t.QuantizedVectorCol))
		}

	case *CreateTableExpr:
		fmtFlags := tree.FmtSimple
		if f.RedactableValues {
			fmtFlags = tree.FmtMarkRedactionNode | tree.FmtOmitNameRedaction
		}
		tp.Child(tree.AsStringWithFlags(t.Syntax, fmtFlags))

	case *CreateViewExpr:
		// Match the format flags used to create t.ViewQuery.
		fmtFlags := tree.FmtParsable | tree.FmtAlwaysQualifyUserDefinedTypeNames
		if f.RedactableValues {
			fmtFlags |= tree.FmtMarkRedactionNode | tree.FmtOmitNameRedaction
		}
		tp.Child(tree.AsStringWithFlags(t.Syntax.AsSource, fmtFlags))

		f.Buffer.Reset()
		f.Buffer.WriteString("columns:")
		for _, col := range t.Columns {
			f.space()
			f.formatCol(col.Alias, col.ID, opt.ColSet{} /* notNullCols */)
		}
		tp.Child(f.Buffer.String())
		f.formatDependencies(tp, t.Deps, t.TypeDeps, t.FuncDeps)

	case *CreateFunctionExpr:
		fmtFlags := tree.FmtSimple
		if f.RedactableValues {
			fmtFlags = tree.FmtMarkRedactionNode | tree.FmtOmitNameRedaction
		}
		tp.Child(tree.AsStringWithFlags(t.Syntax, fmtFlags))
		f.formatDependencies(tp, t.Deps, t.TypeDeps, t.FuncDeps)

	case *CreateTriggerExpr:
		tp.Child(t.Syntax.String())
		f.formatDependencies(tp, t.Deps, t.TypeDeps, t.FuncDeps)

	case *CreateStatisticsExpr:
		tp.Child(t.Syntax.String())

	case *ExportExpr:
		tp.Childf("format: %s", t.FileFormat)

	case *ExplainExpr:
		// ExplainPlan is the default, don't show it.
		m := ""
		if t.Options.Mode != tree.ExplainPlan {
			m = strings.ToLower(t.Options.Mode.String())
		}
		if t.Options.Flags[tree.ExplainFlagVerbose] {
			if m != "" {
				m += ", "
			}
			m += "verbose"
		}
		if m != "" {
			tp.Childf("mode: %s", m)
		}

	case *RecursiveCTEExpr:
		if !f.HasFlags(ExprFmtHideColumns) {
			if t.Deduplicate {
				tp.Childf("deduplicate")
			}
			tp.Childf("working table binding: &%d", t.WithID)
			f.formatRelColList(e, tp, "initial columns:", t.InitialCols)
			f.formatRelColList(e, tp, "recursive columns:", t.RecursiveCols)
		}

	default:
		if opt.IsJoinOp(t) {
			p := t.Private().(*JoinPrivate)
			if !p.Flags.Empty() {
				tp.Childf("flags: %s", p.Flags.String())
			}
		}
	}

	if !f.HasFlags(ExprFmtHideMiscProps) {
		if !relational.OuterCols.Empty() {
			tp.Childf("outer: %s", relational.OuterCols.String())
		}
		if relational.Cardinality != props.AnyCardinality {
			// Suppress cardinality for Scan ops if it's redundant with Limit field.
			if scan, ok := e.(*ScanExpr); !ok || !scan.HardLimit.IsSet() {
				tp.Childf("cardinality: %s", relational.Cardinality)
			}
		}

		if join, ok := e.(joinWithMultiplicity); ok {
			mult := join.getMultiplicity()
			if s := mult.Format(e.Op()); s != "" {
				tp.Childf("multiplicity: %s", s)
			}
		}

		f.Buffer.Reset()
		writeFlag := func(name string) {
			if f.Buffer.Len() != 0 {
				f.Buffer.WriteString(", ")
			}
			f.Buffer.WriteString(name)
		}

		if !relational.VolatilitySet.IsLeakproof() {
			writeFlag(relational.VolatilitySet.String())
		}
		if relational.CanMutate {
			writeFlag("mutations")
		}
		if relational.HasPlaceholder {
			writeFlag("has-placeholder")
		}
		if lookupJoin, ok := e.(*LookupJoinExpr); ok {
			// For lookup joins, indicate whether reverse scans are required to
			// satisfy the ordering.
			if lookupJoinMustUseReverseScans(md, lookupJoin, &required.Ordering) {
				writeFlag("reverse-scans")
			}
		}

		if f.Buffer.Len() != 0 {
			tp.Child(f.Buffer.String())
		}
	}

	if !f.HasFlags(ExprFmtHideStats) {
		if f.HasFlags(ExprFmtHideHistograms) || f.RedactableValues {
			tp.Childf("stats: %s", relational.Statistics().StringWithoutHistograms())
		} else {
			tp.Childf("stats: %s", relational.Statistics())
		}
	}

	if !f.HasFlags(ExprFmtHideCost) {
		cost := e.Cost()
		if cost.C != 0 {
			tp.Childf("cost: %.9g", cost.C)
		}
		if !cost.Flags.Empty() {
			var b strings.Builder
			b.WriteString("cost-flags:")
			if cost.Flags.FullScanPenalty {
				b.WriteString(" full-scan-penalty")
			}
			if cost.Flags.HugeCostPenalty {
				b.WriteString(" huge-cost-penalty")
			}
			if cost.Flags.UnboundedCardinality {
				b.WriteString(" unbounded-cardinality")
			}
			tp.Child(b.String())
		}
	}

	// Format functional dependencies.
	if !f.HasFlags(ExprFmtHideFuncDeps) {
		// Show the key separately from the rest of the FDs.
		if key, ok := relational.FuncDeps.StrictKey(); ok {
			tp.Childf("key: %s", key)
		} else if key, ok := relational.FuncDeps.LaxKey(); ok {
			tp.Childf("lax-key: %s", key)
		}
		if fdStr := relational.FuncDeps.StringOnlyFDs(); fdStr != "" {
			tp.Childf("fd: %s", fdStr)
		}
	}

	if !f.HasFlags(ExprFmtHidePhysProps) {
		if !required.Ordering.Any() {
			if f.HasFlags(ExprFmtHideMiscProps) {
				tp.Childf("ordering: %s", required.Ordering.String())
			} else if e.ProvidedPhysical() != nil {
				// Show the provided ordering as well, unless it's exactly the same.
				provided := e.ProvidedPhysical().Ordering
				reqStr := required.Ordering.String()
				provStr := provided.String()
				if provStr == reqStr {
					tp.Childf("ordering: %s", reqStr)
				} else {
					tp.Childf("ordering: %s [actual: %s]", reqStr, provStr)
				}
			}
		}
		if required.LimitHint != 0 {
			tp.Childf("limit hint: %.2f", required.LimitHint)
		}

		// Show the required distribution, if any, and also show the provided input
		// distribution if this is a Distribute expression.
		if !required.Distribution.Any() {
			tp.Childf("distribution: %s", required.Distribution.String())
		}
		// Show the lookup table distribution of a lookup join, if it has been set.
		if lookupJoinExpr, ok := e.(*LookupJoinExpr); ok && f.Catalog != nil {
			if optimizer := f.Catalog.Optimizer(); optimizer != nil {
				providedDistribution := GetLookupJoinLookupTableDistribution(
					lookupJoinExpr,
					lookupJoinExpr.RequiredPhysical(),
					optimizer,
				)
				if !providedDistribution.Any() {
					tp.Childf("lookup table distribution: %s", providedDistribution.String())
				}
			}
		}
		if distribute, ok := e.(*DistributeExpr); ok {
			tp.Childf("input distribution: %s", distribute.Input.ProvidedPhysical().Distribution.String())
		}
	}

	if !f.HasFlags(ExprFmtHideRuleProps) {
		r := &relational.Rule
		if !r.PruneCols.Empty() {
			tp.Childf("prune: %s", r.PruneCols.String())
		}
		if !r.RejectNullCols.Empty() {
			tp.Childf("reject-nulls: %s", r.RejectNullCols.String())
		}
		if len(r.InterestingOrderings) > 0 {
			tp.Childf("interesting orderings: %s", r.InterestingOrderings.String())
		}
		if !r.UnfilteredCols.Empty() {
			tp.Childf("unfiltered-cols: %s", r.UnfilteredCols.String())
		}
		if withUses := relational.Shared.Rule.WithUses; len(withUses) > 0 {
			n := tp.Childf("cte-uses")
			ids := make([]opt.WithID, 0, len(withUses))
			for id := range withUses {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool {
				return ids[i] < ids[j]
			})
			for _, id := range ids {
				info := withUses[id]
				n.Childf("&%d: count=%d used-columns=%s", id, info.Count, info.UsedCols)
			}
		}
	}

	switch t := e.(type) {
	case *CreateTableExpr:
		// Do not print dummy input expression if there was no AS clause.
		if !t.Syntax.As() {
			return
		}

	case *PlaceholderScanExpr:
		// Show the child scalar expressions under a "span" heading.
		tp = tp.Childf("span")
	}

	for i, n := 0, e.ChildCount(); i < n; i++ {
		f.formatExpr(e.Child(i), tp)
	}
}

// lookupJoinMustUseReverseScans returns true if the given lookup join must use
// reverse scans in its lookups in order to satisfy the required ordering.
// It assumes that the required ordering *can* be satisfied by the lookup join.
func lookupJoinMustUseReverseScans(
	md *opt.Metadata, lookupJoin *LookupJoinExpr, requiredOrdering *props.OrderingChoice,
) bool {
	inputCols := lookupJoin.Input.Relational().OutputCols
	for _, ordColChoice := range requiredOrdering.Columns {
		if ordColChoice.Group.Intersects(inputCols) {
			// This is part of the prefix that can be satisfied by the input.
			continue
		}
		// Forward scans if the index is ordered in the same direction as the
		// required ordering, reverse scans otherwise.
		idx := md.Table(lookupJoin.Table).Index(lookupJoin.Index)
		return ordColChoice.Descending != idx.Column(0).Descending
	}
	return false
}

func (f *ExprFmtCtx) formatScalar(scalar opt.ScalarExpr, tp treeprinter.Node) {
	f.formatScalarWithLabel("", scalar, tp)
}

func (f *ExprFmtCtx) formatScalarWithLabel(
	label string, scalar opt.ScalarExpr, tp treeprinter.Node,
) {
	formatUDFDefinition := func(def *UDFDefinition, tp treeprinter.Node) {
		if _, seen := f.seenUDFs[def]; !seen {
			// Ensure that the definition of the UDF is not printed out again if it
			// is recursively called.
			f.seenUDFs[def] = struct{}{}
			if len(def.Params) > 0 {
				f.formatColList(tp, "params:", def.Params, opt.ColSet{} /* notNullCols */)
			}
			n := tp.Child("body")
			for i := range def.Body {
				stmtNode := n
				if i == 0 {
					if def.FirstStmtOutput.CursorDeclaration != nil {
						// The first statement is opening a cursor.
						stmtNode = n.Child("open-cursor")
					} else if def.FirstStmtOutput.TargetBufferID != 0 {
						// The first statement is writing to a target buffer.
						stmtNode = n.Child("add-to-srf-result")
					}
				}
				prevTailCalls := f.tailCalls

				// Routine calls in the last body statement may be tail calls if
				// ResultBufferID is unset. If it is set, the result of the last body
				// statement is not directly used as the result of the UDF call, so it
				// cannot contain tail calls.
				if i == len(def.Body)-1 && def.ResultBufferID == 0 {
					f.tailCalls = make(map[opt.ScalarExpr]struct{})
					ExtractTailCalls(def.Body[i], f.tailCalls)
				}
				f.formatExpr(def.Body[i], stmtNode)
				f.tailCalls = prevTailCalls
			}
			delete(f.seenUDFs, def)
		} else {
			tp.Child("recursive-call")
		}
		if def.ExceptionBlock != nil {
			n := tp.Child("exception-handler")
			for i := range def.ExceptionBlock.Codes {
				code := def.ExceptionBlock.Codes[i]
				body := def.ExceptionBlock.Actions[i].Body
				var branch treeprinter.Node
				if code.String() == "OTHERS" {
					branch = n.Child("OTHERS")
				} else {
					branch = n.Childf("SQLSTATE '%s'", code)
				}
				for j := range body {
					f.formatExpr(body[j], branch)
				}
			}
		}
	}
	formatRoutineArgs := func(args ScalarListExpr, tp treeprinter.Node) {
		if len(args) > 0 {
			n := tp.Child("args")
			for i := range args {
				f.formatExpr(args[i], n)
			}
		}
	}
	formatUDFInputAndBody := func(udf *UDFCallExpr, tp treeprinter.Node) {
		if !udf.Def.CalledOnNullInput {
			tp.Child("strict")
		}
		if _, tailCall := f.tailCalls[udf]; tailCall {
			// This routine is in tail-call position in the parent routine.
			tp.Child("tail-call")
		}
		formatRoutineArgs(udf.Args, tp)
		formatUDFDefinition(udf.Def, tp)
	}

	f.Buffer.Reset()
	if label != "" {
		f.Buffer.WriteString(label)
		f.Buffer.WriteString(": ")
	}
	switch scalar.Op() {
	case opt.ProjectionsOp, opt.AggregationsOp, opt.UniqueChecksOp, opt.FKChecksOp, opt.KVOptionsOp,
		opt.FastPathUniqueChecksOp:
		// Omit empty lists (except filters) and special-purpose fast path check expressions.
		if scalar.ChildCount() == 0 ||
			(scalar.Op() == opt.FastPathUniqueChecksOp && f.HasFlags(ExprFmtHideFastPathChecks)) {
			return
		}

	case opt.FiltersOp:
		// Show empty Filters expression as "filters (true)".
		if scalar.ChildCount() == 0 {
			f.Buffer.WriteString("filters (true)")
			tp.Child(f.Buffer.String())
			return
		}

	case opt.IfErrOp:
		fmt.Fprintf(f.Buffer, "%v", scalar.Op())
		f.FormatScalarProps(scalar)

		tp = tp.Child(f.Buffer.String())

		f.formatExpr(scalar.Child(0), tp)
		if scalar.Child(1).ChildCount() > 0 {
			f.formatExpr(scalar.Child(1), tp.Child("else"))
		}
		if scalar.Child(2).ChildCount() > 0 {
			f.formatExpr(scalar.Child(2), tp.Child("err-code"))
		}

		return

	case opt.AggFilterOp:
		fmt.Fprintf(f.Buffer, "%v", scalar.Op())
		f.FormatScalarProps(scalar)
		tp = tp.Child(f.Buffer.String())

		f.formatExpr(scalar.Child(0), tp)
		f.formatExpr(scalar.Child(1), tp.Child("filter"))

		return

	case opt.ScalarListOp:
		// Don't show scalar-list as a separate node, as it's redundant with its
		// parent.
		for i, n := 0, scalar.ChildCount(); i < n; i++ {
			f.formatExpr(scalar.Child(i), tp)
		}
		return

	case opt.UDFCallOp:
		udf := scalar.(*UDFCallExpr)
		fmt.Fprintf(f.Buffer, "%s: %s", udf.Def.RoutineType, udf.Def.Name)
		f.FormatScalarProps(scalar)
		tp = tp.Child(f.Buffer.String())
		formatUDFInputAndBody(udf, tp)
		return

	case opt.TxnControlOp:
		controlExpr := scalar.(*TxnControlExpr)
		fmt.Fprintf(f.Buffer, "%s; CALL %s", controlExpr.TxnOp, controlExpr.Def.Name)
		f.FormatScalarProps(scalar)
		tp = tp.Child(f.Buffer.String())
		formatRoutineArgs(controlExpr.Args, tp)
		formatUDFDefinition(controlExpr.Def, tp)
	}

	// Omit various list items from the output, but show some of their properties
	// along with the properties of their child.
	var scalarProps []string
	switch scalar.Op() {
	case opt.FiltersItemOp, opt.ProjectionsItemOp, opt.AggregationsItemOp,
		opt.ZipItemOp, opt.WindowsItemOp:

		emitProp := func(format string, args ...interface{}) {
			scalarProps = append(scalarProps, fmt.Sprintf(format, args...))
		}
		switch item := scalar.(type) {
		case *ProjectionsItem:
			if !f.HasFlags(ExprFmtHideColumns) {
				emitProp("as=%s", f.ColumnString(item.Col))
			}

		case *AggregationsItem:
			if !f.HasFlags(ExprFmtHideColumns) {
				emitProp("as=%s", f.ColumnString(item.Col))
			}

		case *ZipItem:
			// TODO(radu): show the item.Cols

		case *WindowsItem:
			if !f.HasFlags(ExprFmtHideColumns) {
				emitProp("as=%s", f.ColumnString(item.Col))
			}
			// Only show the frame if it differs from the default.
			def := WindowFrame{
				Mode:           treewindow.RANGE,
				StartBoundType: treewindow.UnboundedPreceding,
				EndBoundType:   treewindow.CurrentRow,
				FrameExclusion: treewindow.NoExclusion,
			}
			if item.Frame != def {
				emitProp("frame=%q", item.Frame.String())
			}
		}

		scalarProps = append(scalarProps, f.scalarPropsStrings(scalar)...)
		scalar = scalar.Child(0).(opt.ScalarExpr)

	default:
		scalarProps = f.scalarPropsStrings(scalar)
	}

	var intercepted bool
	if !f.HasFlags(ExprFmtHideScalars) {
		switch t := scalar.(type) {
		case *UDFCallExpr:
			// A UDF function body will be printed after the scalar props, so
			// pre-emptively set intercepted=true to avoid the default
			// formatScalarPrivate formatting below.
			fmt.Fprintf(f.Buffer, "udf: %s", t.Def.Name)
			intercepted = true
		case *TxnControlExpr:
			// As for UDFCallExpr, the arguments and body will be printed below.
			fmt.Fprintf(f.Buffer, "%s; CALL %s", t.TxnOp, t.Def.Name)
			intercepted = true
		}
	}
	if !intercepted && f.HasFlags(ExprFmtHideScalars) && ScalarFmtInterceptor != nil {
		if str := ScalarFmtInterceptor(f, scalar); str != "" {
			f.Buffer.WriteString(str)
			intercepted = true
		}
	}
	if !intercepted {
		fmt.Fprintf(f.Buffer, "%v", scalar.Op())
		f.formatScalarPrivate(scalar)
	}
	if len(scalarProps) != 0 {
		f.Buffer.WriteString(" [")
		f.Buffer.WriteString(strings.Join(scalarProps, ", "))
		f.Buffer.WriteByte(']')
	}
	tp = tp.Child(f.Buffer.String())

	if !f.HasFlags(ExprFmtHideScalars) {
		switch t := scalar.(type) {
		case *UDFCallExpr:
			formatUDFInputAndBody(t, tp)
		case *TxnControlExpr:
			formatRoutineArgs(t.Args, tp)
			formatUDFDefinition(t.Def, tp)
		case *SubqueryExpr:
			if _, tailCall := f.tailCalls[t]; tailCall {
				// Subqueries nested within routines are themselves planned as nested
				// routines. This subquery is in tail-call position in the parent
				// routine.
				tp.Child("tail-call")
			}
			prevTailCalls := f.tailCalls
			f.tailCalls = make(map[opt.ScalarExpr]struct{})
			ExtractTailCalls(t.Input, f.tailCalls)
			f.formatExpr(t.Input, tp)
			f.tailCalls = prevTailCalls
			intercepted = true
		}
	}

	if !intercepted {
		for i, n := 0, scalar.ChildCount(); i < n; i++ {
			f.formatExpr(scalar.Child(i), tp)
		}
	}
}

// scalarPropsStrings returns a slice of strings, each describing a property;
// for example:
//
//	{"type=bool", "outer=(1)", "constraints=(/1: [/1 - /1]; tight)"}
func (f *ExprFmtCtx) scalarPropsStrings(scalar opt.ScalarExpr) []string {
	typ := scalar.DataType()
	if typ == nil {
		if scalar.Op() == opt.UniqueChecksItemOp || scalar.Op() == opt.FKChecksItemOp ||
			scalar.Op() == opt.KVOptionsItemOp || scalar.Op() == opt.FastPathUniqueChecksItemOp {
			// These are not true scalars and have no properties.
			return nil
		}
		// Don't panic if scalar properties don't yet exist when printing
		// expression.
		return []string{"type=undefined"}
	}

	var res []string
	emitProp := func(format string, args ...interface{}) {
		res = append(res, fmt.Sprintf(format, args...))
	}
	if !f.HasFlags(ExprFmtHideTypes) && typ.Family() != types.AnyFamily {
		emitProp("type=%s", typ)
	}
	if propsExpr, ok := scalar.(ScalarPropsExpr); ok {
		scalarProps := propsExpr.ScalarProps()
		if !f.HasFlags(ExprFmtHideMiscProps) {
			if !scalarProps.OuterCols.Empty() {
				emitProp("outer=%s", scalarProps.OuterCols)
			}
			if !scalarProps.VolatilitySet.IsLeakproof() {
				emitProp(scalarProps.VolatilitySet.String())
			}
			if scalarProps.HasCorrelatedSubquery {
				emitProp("correlated-subquery")
			} else if scalarProps.HasSubquery {
				emitProp("subquery")
			}
			if scalarProps.HasUDF {
				emitProp("udf")
			}
		}

		if !f.HasFlags(ExprFmtHideConstraints) {
			if scalarProps.Constraints != nil && !scalarProps.Constraints.IsUnconstrained() {
				var tight string
				if scalarProps.TightConstraints {
					tight = "; tight"
				}
				emitProp(
					"constraints=(%s%s)",
					cat.MaybeMarkRedactable(scalarProps.Constraints.String(), f.RedactableValues), tight,
				)
			}
		}

		if !f.HasFlags(ExprFmtHideFuncDeps) && !scalarProps.FuncDeps.Empty() {
			emitProp("fd=%s", scalarProps.FuncDeps)
		}
	}
	return res
}

// FormatScalarProps writes out a string representation of the scalar
// properties (with a preceding space); for example:
//
//	" [type=bool, outer=(1), constraints=(/1: [/1 - /1]; tight)]"
func (f *ExprFmtCtx) FormatScalarProps(scalar opt.ScalarExpr) {
	props := f.scalarPropsStrings(scalar)
	if len(props) != 0 {
		f.Buffer.WriteString(" [")
		f.Buffer.WriteString(strings.Join(props, ", "))
		f.Buffer.WriteByte(']')
	}
}

func (f *ExprFmtCtx) formatScalarPrivate(scalar opt.ScalarExpr) {
	var private interface{}
	switch t := scalar.(type) {
	case *NullExpr, *TupleExpr:
		// Private is redundant with logical type property.
		private = nil

	case *CollateExpr:
		fmt.Fprintf(f.Buffer, " locale='%s'", t.Locale)

	case *AnyExpr:
		// We don't want to show the OriginalExpr; just show Cmp.
		private = t.Cmp

	case *ArrayFlattenExpr:
		if t.Input.Relational().OutputCols.Len() != 1 {
			fmt.Fprintf(f.Buffer, " col=%v", t.RequestedCol)
		}

	case *SubqueryExpr, *ExistsExpr:
		// We don't want to show the OriginalExpr.
		private = nil

	case *CastExpr, *AssignmentCastExpr:
		typ := scalar.Private().(*types.T)
		private = typ.SQLString()

	case *KVOptionsItem:
		fmt.Fprintf(f.Buffer, " %s", t.Key)

	case *UniqueChecksItem:
		tab := f.Memo.metadata.TableMeta(t.Table)
		constraint := tab.Table.Unique(t.CheckOrdinal)
		fmt.Fprintf(f.Buffer, ": %s(", tab.Alias.ObjectName)
		for i := 0; i < constraint.ColumnCount(); i++ {
			if i > 0 {
				f.Buffer.WriteByte(',')
			}
			col := tab.Table.Column(constraint.ColumnOrdinal(tab.Table, i))
			f.Buffer.WriteString(string(col.ColName()))
		}
		f.Buffer.WriteByte(')')

	case *FastPathUniqueChecksItem:
		tab := f.Memo.metadata.TableMeta(t.ReferencedTableID)
		constraint := tab.Table.Unique(t.CheckOrdinal)
		fmt.Fprintf(f.Buffer, ": %s(", tab.Alias.ObjectName)
		for i := 0; i < constraint.ColumnCount(); i++ {
			if i > 0 {
				f.Buffer.WriteByte(',')
			}
			col := tab.Table.Column(constraint.ColumnOrdinal(tab.Table, i))
			f.Buffer.WriteString(string(col.ColName()))
		}
		f.Buffer.WriteByte(')')

	case *FKChecksItem:
		origin := f.Memo.metadata.TableMeta(t.OriginTable)
		referenced := f.Memo.metadata.TableMeta(t.ReferencedTable)
		var fk cat.ForeignKeyConstraint
		if t.FKOutbound {
			fk = origin.Table.OutboundForeignKey(t.FKOrdinal)
		} else {
			fk = referenced.Table.InboundForeignKey(t.FKOrdinal)
		}
		// Print the FK as:
		//   child(a,b) -> parent(a,b)
		//
		// TODO(radu): maybe flip these if we are deleting from the parent (i.e.
		// FKOutbound=false)?
		fmt.Fprintf(f.Buffer, ": %s(", origin.Alias.ObjectName)
		for i := 0; i < fk.ColumnCount(); i++ {
			if i > 0 {
				f.Buffer.WriteByte(',')
			}
			col := origin.Table.Column(fk.OriginColumnOrdinal(origin.Table, i))
			f.Buffer.WriteString(string(col.ColName()))
		}
		fmt.Fprintf(f.Buffer, ") -> %s(", referenced.Alias.ObjectName)
		for i := 0; i < fk.ColumnCount(); i++ {
			if i > 0 {
				f.Buffer.WriteByte(',')
			}
			col := referenced.Table.Column(fk.ReferencedColumnOrdinal(referenced.Table, i))
			f.Buffer.WriteString(string(col.ColName()))
		}
		f.Buffer.WriteByte(')')

	case *UDFCallExpr:
		private = nil

	default:
		private = scalar.Private()
	}

	if private != nil {
		f.Buffer.WriteRune(':')
		FormatPrivate(f, private, &physical.Required{})
	}
}

// formatIndex outputs the specified index into the context's buffer with the
// format:
//
//	table_alias@index_name
//
// If reverse is true, ",rev" is appended.
//
// If the index is a partial index, ",partial" is appended.
//
// If the table is aliased, " [as=alias]" is appended.
func (f *ExprFmtCtx) formatIndex(tabID opt.TableID, idxOrd cat.IndexOrdinal, reverse bool) {
	md := f.Memo.Metadata()
	tabMeta := md.TableMeta(tabID)
	index := tabMeta.Table.Index(idxOrd)
	if idxOrd == cat.PrimaryIndex {
		// Don't output the index name if it's the primary index.
		fmt.Fprintf(f.Buffer, " %s", tableName(f, tabID))
	} else {
		fmt.Fprintf(f.Buffer, " %s@%s", tableName(f, tabID), index.Name())
	}
	if reverse {
		f.Buffer.WriteString(",rev")
	}
	switch index.Type() {
	case idxtype.INVERTED:
		f.Buffer.WriteString(",inverted")
	case idxtype.VECTOR:
		f.Buffer.WriteString(",vector")
	}
	if _, isPartial := index.Predicate(); isPartial {
		f.Buffer.WriteString(",partial")
	}
	alias := md.TableMeta(tabID).Alias.Table()
	if alias != string(tabMeta.Table.Name()) {
		fmt.Fprintf(f.Buffer, " [as=%s]", alias)
	}
}

// formatIndexes constructs a new treeprinter child containing the
// specified list of indexes with the label specified.
func (f *ExprFmtCtx) formatIndexes(
	tp treeprinter.Node, label string, indexes cat.IndexOrdinals, tabID opt.TableID,
) {
	md := f.Memo.Metadata()
	tab := md.Table(tabID)

	if len(indexes) > 0 {
		f.Buffer.Reset()
		f.Buffer.WriteString(fmt.Sprintf("%s indexes:", label))
		for _, idx := range indexes {
			name := string(tab.Index(idx).Name())
			f.space()
			f.Buffer.WriteString(name)
		}
		tp.Child(f.Buffer.String())
	}
}

// formatArbiterConstraints constructs a new treeprinter child containing the
// specified list of arbiter constraints.
func (f *ExprFmtCtx) formatArbiterConstraints(
	tp treeprinter.Node, arbiters cat.UniqueOrdinals, tabID opt.TableID,
) {
	md := f.Memo.Metadata()
	tab := md.Table(tabID)

	if len(arbiters) > 0 {
		f.Buffer.Reset()
		f.Buffer.WriteString("arbiter constraints:")
		for _, uc := range arbiters {
			name := tab.Unique(uc).Name()
			f.space()
			f.Buffer.WriteString(name)
		}
		tp.Child(f.Buffer.String())
	}
}

func (f *ExprFmtCtx) formatColumns(
	nd RelExpr, tp treeprinter.Node, cols opt.ColList, presentation physical.Presentation,
) {
	if f.HasFlags(ExprFmtHideColumns) {
		return
	}
	if presentation.Any() {
		f.formatRelColList(nd, tp, "columns:", cols)
		return
	}

	// When a particular column presentation is required of the expression, then
	// print columns using that information. Include information about columns
	// that are hidden by the presentation separately.
	hidden := cols.ToSet()
	notNullCols := nd.Relational().NotNullCols
	f.Buffer.Reset()
	f.Buffer.WriteString("columns:")
	for _, col := range presentation {
		hidden.Remove(col.ID)
		f.space()
		f.formatCol(col.Alias, col.ID, notNullCols)
	}
	if !hidden.Empty() {
		f.Buffer.WriteString("  [hidden:")
		for _, col := range cols {
			if hidden.Contains(col) {
				f.space()
				f.formatCol("" /* label */, col, notNullCols)
			}
		}
		f.Buffer.WriteString("]")
	}
	tp.Child(f.Buffer.String())
}

// formatRelColList constructs a new treeprinter child containing the specified
// list of columns formatted using the formatCol method.
func (f *ExprFmtCtx) formatRelColList(
	nd RelExpr, tp treeprinter.Node, heading string, colList opt.ColList,
) {
	f.formatColList(tp, heading, colList, nd.Relational().NotNullCols)
}

// formatColList constructs a new treeprinter child containing the specified
// list of columns formatted using the formatCol method.
func (f *ExprFmtCtx) formatColList(
	tp treeprinter.Node, heading string, colList opt.ColList, notNullCols opt.ColSet,
) {
	if len(colList) > 0 {
		f.Buffer.Reset()
		f.Buffer.WriteString(heading)
		for _, col := range colList {
			f.space()
			f.formatCol("" /* label */, col, notNullCols)
		}
		tp.Child(f.Buffer.String())
	}
}

// formatOptionalColList constructs a new treeprinter child containing the
// specified list of optional columns formatted using the formatCol method.
func (f *ExprFmtCtx) formatOptionalColList(
	nd RelExpr, tp treeprinter.Node, heading string, colList opt.OptionalColList,
) {
	if !colList.IsEmpty() {
		notNullCols := nd.Relational().NotNullCols
		f.Buffer.Reset()
		f.Buffer.WriteString(heading)
		for _, col := range colList {
			if col != 0 {
				f.space()
				f.formatCol("" /* label */, col, notNullCols)
			}
		}
		tp.Child(f.Buffer.String())
	}
}

// formatMutationCols adds a new treeprinter child for each non-zero column in the
// given list. Each child shows how the column will be mutated, with the id of
// the "before" and "after" columns, similar to this:
//
//	a:1 => x:4
func (f *ExprFmtCtx) formatMutationCols(
	nd RelExpr, tp treeprinter.Node, heading string, colList opt.OptionalColList, tabID opt.TableID,
) {
	if colList.IsEmpty() {
		return
	}

	tpChild := tp.Child(heading)
	for i, col := range colList {
		if col != 0 {
			tpChild.Child(fmt.Sprintf("%s => %s", f.ColumnString(col), f.ColumnString(tabID.ColumnID(i))))
		}
	}
}

// formatMutationCommon shows the MutationPrivate fields that format the same
// for all types of mutations.
func (f *ExprFmtCtx) formatMutationCommon(tp treeprinter.Node, p *MutationPrivate) {
	if p.WithID != 0 {
		tp.Childf("input binding: &%d", p.WithID)
	}
	if len(p.FKCascades) > 0 {
		c := tp.Childf("cascades")
		for i := range p.FKCascades {
			c.Child(p.FKCascades[i].FKConstraint.Name())
		}
	}
	if p.AfterTriggers != nil {
		c := tp.Childf("after-triggers")
		for i := range p.AfterTriggers.Triggers {
			c.Child(p.AfterTriggers.Triggers[i].Name().Normalize())
		}
	}
}

// formatBeforeTriggers displays the names of BEFORE triggers that will be
// executed for the given table and mutation event types.
func (f *ExprFmtCtx) formatBeforeTriggers(
	tp treeprinter.Node, tableID opt.TableID, events ...tree.TriggerEventType,
) {
	tab := f.Memo.Metadata().Table(tableID)
	triggers := cat.GetRowLevelTriggers(
		tab, tree.TriggerActionTimeBefore, tree.MakeTriggerEventTypeSet(events...),
	)
	if len(triggers) > 0 {
		c := tp.Child("before-triggers")
		for _, trigger := range triggers {
			c.Child(trigger.Name().Normalize())
		}
	}
}

// ColumnString returns the column in the same format as formatColSimple.
func (f *ExprFmtCtx) ColumnString(id opt.ColumnID) string {
	var buf bytes.Buffer
	f.formatColSimpleToBuffer(&buf, "" /* label */, id)
	return buf.String()
}

// formatColSimple outputs the specified column into the context's buffer using the
// following format:
//
//	label:id
//
// The :id part is omitted if the formatting flags include ExprFmtHideColumns.
//
// If a label is given, then it is used. Otherwise, a "best effort" label is
// used from query metadata.
func (f *ExprFmtCtx) formatColSimple(label string, id opt.ColumnID) {
	f.formatColSimpleToBuffer(f.Buffer, label, id)
}

func (f *ExprFmtCtx) formatColSimpleToBuffer(buf *bytes.Buffer, label string, id opt.ColumnID) {
	if label == "" {
		if f.Memo != nil {
			//nolint metadata will never be locked here.
			md := f.Memo.metadata
			fullyQualify := !f.HasFlags(ExprFmtHideQualifications)
			label = md.QualifiedAlias(f.Ctx, id, fullyQualify, false /* alwaysQualify */, f.Catalog)
		} else {
			label = fmt.Sprintf("unknown%d", id)
		}
	}

	if !isSimpleColumnName(label) {
		// Add quotations around the column name if it is not composed of simple
		// ASCII characters.
		label = "\"" + label + "\""
	}

	buf.WriteString(label)
	if !f.HasFlags(ExprFmtHideColumns) {
		buf.WriteByte(':')
		fmt.Fprintf(buf, "%d", id)
	}
}

// formatCol outputs the specified column into the context's buffer using the
// following format:
//
//	label:id(type)
//
// If the column is not nullable, then this is the format:
//
//	label:id(type!null)
//
// Some of the components can be omitted depending on formatting flags.
//
// If a label is given, then it is used. Otherwise, a "best effort" label is
// used from query metadata.
func (f *ExprFmtCtx) formatCol(label string, id opt.ColumnID, notNullCols opt.ColSet) {
	f.formatColSimple(label, id)
	parenOpen := false
	if !f.HasFlags(ExprFmtHideTypes) && f.Memo != nil {
		f.Buffer.WriteByte('(')
		parenOpen = true
		f.Buffer.WriteString(f.Memo.metadata.ColumnMeta(id).Type.String())
	}
	if !f.HasFlags(ExprFmtHideNotNull) && notNullCols.Contains(id) {
		f.Buffer.WriteString("!null")
	}
	if parenOpen {
		f.Buffer.WriteByte(')')
	}
}

// formatLocking adds a new treeprinter child for the row-level locking policy,
// if the policy is configured to perform row-level locking.
func (f *ExprFmtCtx) formatLocking(tp treeprinter.Node, locking opt.Locking) {
	f.formatLockingWithPrefix(tp, "", locking)
}

func (f *ExprFmtCtx) formatLockingWithPrefix(
	tp treeprinter.Node, labelPrefix string, locking opt.Locking,
) {
	if locking.IsNoOp() {
		return
	}
	strength := ""
	switch locking.Strength {
	case tree.ForNone:
		strength = "none"
	case tree.ForKeyShare:
		strength = "for-key-share"
	case tree.ForShare:
		strength = "for-share"
	case tree.ForNoKeyUpdate:
		strength = "for-no-key-update"
	case tree.ForUpdate:
		strength = "for-update"
	default:
		panic(errors.AssertionFailedf("unexpected strength"))
	}
	wait := ""
	switch locking.WaitPolicy {
	case tree.LockWaitBlock:
	case tree.LockWaitSkipLocked:
		wait = ",skip-locked"
	case tree.LockWaitError:
		wait = ",nowait"
	default:
		panic(errors.AssertionFailedf("unexpected wait policy"))
	}
	form := ""
	switch locking.Form {
	case tree.LockRecord:
	case tree.LockPredicate:
		form = ",predicate"
	default:
		panic(errors.AssertionFailedf("unexpected form"))
	}
	durability := ""
	switch locking.Durability {
	case tree.LockDurabilityBestEffort:
	case tree.LockDurabilityGuaranteed:
		durability = ",durability-guaranteed"
	default:
		panic(errors.AssertionFailedf("unexpected durability"))
	}
	tp.Childf("%slocking: %s%s%s%s", labelPrefix, strength, wait, form, durability)
}

// formatDependencies adds a new treeprinter child for schema dependencies.
func (f *ExprFmtCtx) formatDependencies(
	tp treeprinter.Node,
	deps opt.SchemaDeps,
	typeDeps opt.SchemaTypeDeps,
	funcDeps opt.SchemaFunctionDeps,
) {
	if len(deps) == 0 && typeDeps.Empty() && funcDeps.Empty() {
		tp.Child("no dependencies")
		return
	}
	n := tp.Child("dependencies")
	for _, dep := range deps {
		f.Buffer.Reset()
		name := dep.DataSource.Name()
		f.Buffer.WriteString(name.String())
		if dep.SpecificIndex {
			fmt.Fprintf(f.Buffer, "@%s", dep.DataSource.(cat.Table).Index(dep.Index).Name())
		}
		colNames, isTable := dep.GetColumnNames()
		if len(colNames) > 0 {
			fmt.Fprintf(f.Buffer, " [columns:")
			for _, colName := range colNames {
				fmt.Fprintf(f.Buffer, " %s", colName)
			}
			fmt.Fprintf(f.Buffer, "]")
		} else if isTable {
			fmt.Fprintf(f.Buffer, " [no columns]")
		}
		n.Child(f.Buffer.String())
	}
	for _, typ := range f.Memo.Metadata().AllUserDefinedTypes() {
		typeID := catid.UserDefinedOIDToID(typ.Oid())
		if typeDeps.Contains(int(typeID)) {
			n.Child(typ.Name())
		}
	}
	funcDeps.ForEach(func(routineID int) {
		// TODO(drewk): format using the routine name.
		ref := tree.FunctionOID{OID: catid.FuncIDToOID(catid.DescID(routineID))}
		n.Child(ref.String())
	})
}

// ScanIsReverseFn is a callback that is used to figure out if a scan needs to
// happen in reverse (the code lives in the ordering package, and depending on
// that directly would be a dependency loop).
var ScanIsReverseFn func(md *opt.Metadata, s *ScanPrivate, required *props.OrderingChoice) bool

// FormatPrivate outputs a description of the private to f.Buffer.
func FormatPrivate(f *ExprFmtCtx, private interface{}, physProps *physical.Required) {
	if private == nil {
		return
	}
	switch t := private.(type) {
	case *opt.ColumnID:
		f.space()
		f.formatColSimple("" /* label */, *t)

	case *opt.ColList:
		for _, col := range *t {
			f.space()
			f.formatColSimple("" /* label */, col)
		}

	case *TupleOrdinal:
		fmt.Fprintf(f.Buffer, " %d", *t)

	case *ScanPrivate:
		f.formatIndex(t.Table, t.Index, ScanIsReverseFn(f.Memo.Metadata(), t, &physProps.Ordering))

	case *SequenceSelectPrivate:
		seq := f.Memo.metadata.Sequence(t.Sequence)
		fmt.Fprintf(f.Buffer, " %s", seq.Name())

	case *MutationPrivate:
		f.formatIndex(t.Table, cat.PrimaryIndex, false /* reverse */)

	case *LockPrivate:
		f.formatIndex(t.Table, cat.PrimaryIndex, false /* reverse */)

	case *OrdinalityPrivate:
		if !t.Ordering.Any() {
			fmt.Fprintf(f.Buffer, " ordering=%s", t.Ordering)
		}

	case *GroupingPrivate:
		fmt.Fprintf(f.Buffer, " cols=%s", t.GroupingCols.String())
		if !t.Ordering.Any() {
			fmt.Fprintf(f.Buffer, ",ordering=%s", t.Ordering)
		}

	case *SetPrivate:
		if !t.Ordering.Any() {
			fmt.Fprintf(f.Buffer, " ordering=%s", t.Ordering)
		}

	case *IndexJoinPrivate:
		tab := f.Memo.metadata.Table(t.Table)
		fmt.Fprintf(f.Buffer, " %s", tab.Name())

	case *InvertedFilterPrivate:
		col := f.Memo.metadata.ColumnMeta(t.InvertedColumn)
		fmt.Fprintf(f.Buffer, " %s", col.Alias)

	case *LookupJoinPrivate:
		f.formatIndex(t.Table, t.Index, false /* reverse */)

	case *InvertedJoinPrivate:
		f.formatIndex(t.Table, t.Index, false /* reverse */)

	case *ValuesPrivate:
		fmt.Fprintf(f.Buffer, " id=v%d", t.ID)

	case *ZigzagJoinPrivate:
		f.formatIndex(t.LeftTable, t.LeftIndex, false /* reverse */)
		f.formatIndex(t.RightTable, t.RightIndex, false /* reverse */)

	case *MergeJoinPrivate:
		fmt.Fprintf(f.Buffer, " %s,%s,%s", t.JoinType, t.LeftEq, t.RightEq)

	case *FunctionPrivate:
		fmt.Fprintf(f.Buffer, " %s", t.Name)

	case *WindowsItemPrivate:
		fmt.Fprintf(f.Buffer, " frame=%q", &t.Frame)

	case *WindowPrivate:
		fmt.Fprintf(f.Buffer, " partition=%s", t.Partition)
		if !t.Ordering.Any() {
			fmt.Fprintf(f.Buffer, " ordering=%s", t.Ordering)
		}

	case *VectorSearchPrivate:
		f.formatIndex(t.Table, t.Index, false /* reverse */)

	case *VectorMutationSearchPrivate:
		f.formatIndex(t.Table, t.Index, false /* reverse */)

	case *props.OrderingChoice:
		if !t.Any() {
			fmt.Fprintf(f.Buffer, " ordering=%s", t)
		}

	case *OpaqueRelPrivate:
		f.space()
		f.Buffer.WriteString(t.Metadata.String())

	case *AlterTableSplitPrivate:
		f.formatIndex(t.Table, t.Index, false /* reverse */)

	case *AlterTableRelocatePrivate:
		FormatPrivate(f, &t.AlterTableSplitPrivate, nil)
		switch t.SubjectReplicas {
		case tree.RelocateLease:
			f.Buffer.WriteString(" [lease]")
		case tree.RelocateNonVoters:
			f.Buffer.WriteString(" [non-voters]")
		case tree.RelocateVoters:
			f.Buffer.WriteString(" [voters]")
		}

	case *ControlJobsPrivate:
		fmt.Fprintf(f.Buffer, " (%s)", tree.JobCommandToStatement[t.Command])

	case *CancelPrivate:
		if t.IfExists {
			f.Buffer.WriteString(" [if-exists]")
		}

	case *CreateViewPrivate:
		schema := f.Memo.Metadata().Schema(t.Schema)
		fmt.Fprintf(f.Buffer, " %s.%s", schema.Name(), t.Syntax.Name.Table())

	case *JoinPrivate:
		// Nothing to show; flags are shown separately.

	case *ExplainPrivate, *opt.ColSet, *types.T, *ExportPrivate:
		// Don't show anything, because it's mostly redundant.

	default:
		if f.RedactableValues {
			redact.Fprintf(f.Buffer, " %v", private)
		} else {
			fmt.Fprintf(f.Buffer, " %v", private)
		}
	}
}

// tableName returns the table name to be used for pretty-printing. If
// ExprFmtHideQualifications is not set, the fully qualified table name is
// returned.
func tableName(f *ExprFmtCtx, tabID opt.TableID) string {
	tabMeta := f.Memo.metadata.TableMeta(tabID)
	if f.HasFlags(ExprFmtHideQualifications) {
		return string(tabMeta.Table.Name())
	}
	tn, err := f.Catalog.FullyQualifiedName(f.Ctx, tabMeta.Table)
	if err != nil {
		panic(err)
	}
	return tn.FQString()
}

// isSimpleColumnName returns true if the given label consists of only ASCII
// letters, numbers, underscores, quotation marks, and periods ("."). It is
// used to determine whether to enclose a column name in quotation marks for
// nicer display.
func isSimpleColumnName(label string) bool {
	for i, r := range label {
		if r > unicode.MaxASCII {
			return false
		}

		if i == 0 {
			if r != '"' && !unicode.IsLetter(r) {
				// The first character must be a letter or quotation mark.
				return false
			}
		} else if r != '.' && r != '_' && r != '"' && !unicode.IsNumber(r) && !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}
