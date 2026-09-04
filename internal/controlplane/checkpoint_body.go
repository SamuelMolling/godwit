package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	pgquery "github.com/pganalyze/pg_query_go/v6"
	"github.com/stripe/pg-schema-diff/pkg/diff"
	"github.com/stripe/pg-schema-diff/pkg/tempdb"
)

// renderForEmptyDatabase renders the schema for what a checkpoint body meets: an empty scratch database or
// a target with no history, neither of which has rows, readers or writers for the online shape to protect.
func renderForEmptyDatabase(ctx context.Context, from, to *sql.DB, factory tempdb.Factory) (string, error) {
	ddl, err := generate(ctx, from, to, factory, diff.WithNoConcurrentIndexOps())
	if err != nil {
		return "", err
	}

	return foldStatements(ddl)
}

type bodyStmt struct {
	sql     string
	node    *pgquery.Node
	dropped bool
}

// foldStatements collapses the two shapes pg-schema-diff spreads over several statements so a populated
// table keeps serving: the index a constraint then adopts, and the constraint added NOT VALID then
// validated. Anything the fold cannot reproduce exactly is left as the generator wrote it.
func foldStatements(ddl string) (string, error) {
	stmts, err := bodyStatements(ddl)
	if err != nil {
		return "", err
	}
	tables, indexes, invalid := map[string]int{}, map[string]int{}, map[string]int{}
	for i, st := range stmts {
		indexStatement(st, i, tables, indexes, invalid)
	}
	for i := range stmts {
		foldAdoptedIndex(stmts, i, tables, indexes)
		foldValidation(stmts, i, invalid)
	}
	kept := make([]string, 0, len(stmts))
	for _, st := range stmts {
		if !st.dropped {
			kept = append(kept, st.sql)
		}
	}

	return strings.Join(kept, ";\n") + ";", nil
}

func bodyStatements(ddl string) ([]bodyStmt, error) {
	res, err := pgquery.Parse(ddl)
	if err != nil {
		return nil, fmt.Errorf("parse the rendered schema: %w", err)
	}
	out := make([]bodyStmt, 0, len(res.Stmts))
	for _, raw := range res.Stmts {
		end := len(ddl)
		if raw.GetStmtLen() > 0 {
			end = int(raw.GetStmtLocation() + raw.GetStmtLen())
		}
		out = append(out, bodyStmt{sql: strings.TrimSpace(ddl[raw.GetStmtLocation():end]), node: raw.GetStmt()})
	}

	return out, nil
}

func indexStatement(st bodyStmt, i int, tables, indexes, invalid map[string]int) {
	if ct := st.node.GetCreateStmt(); ct != nil && ct.GetPartspec() == nil && ct.GetPartbound() == nil {
		tables[relKey(ct.GetRelation())] = i
	}
	if ix := st.node.GetIndexStmt(); ix != nil {
		indexes[relKey(ix.GetRelation())+"."+ix.GetIdxname()] = i
	}
	if rel, con := addedConstraint(st.node); con != nil && con.GetSkipValidation() && con.GetConname() != "" {
		invalid[relKey(rel)+"."+con.GetConname()] = i
	}
}

func foldAdoptedIndex(stmts []bodyStmt, i int, tables, indexes map[string]int) {
	rel, con := adoptedConstraint(stmts[i].node)
	if con == nil {
		return
	}
	at, hasIndex := indexes[relKey(rel)+"."+con.GetIndexname()]
	tbl, hasTable := tables[relKey(rel)]
	if !hasIndex || !hasTable {
		return
	}
	clause, ok := constraintClause(stmts[at].node.GetIndexStmt(), con)
	head, closed := strings.CutSuffix(stmts[tbl].sql, ")")
	head = strings.TrimRight(head, " \t\r\n")
	if !ok || !closed || strings.HasSuffix(head, "(") {
		return
	}
	stmts[tbl].sql = head + ",\n\t" + clause + "\n)"
	stmts[at].dropped, stmts[i].dropped = true, true
}

func foldValidation(stmts []bodyStmt, i int, invalid map[string]int) {
	a := stmts[i].node.GetAlterTableStmt()
	if a == nil || len(a.GetCmds()) != 1 {
		return
	}
	cmd := a.GetCmds()[0].GetAlterTableCmd()
	if cmd.GetSubtype() != pgquery.AlterTableType_AT_ValidateConstraint {
		return
	}
	at, ok := invalid[relKey(a.GetRelation())+"."+cmd.GetName()]
	if !ok {
		return
	}
	head, cut := strings.CutSuffix(stmts[at].sql, " NOT VALID")
	if !cut {
		return
	}
	stmts[at].sql = head
	stmts[i].dropped = true
}

func addedConstraint(node *pgquery.Node) (*pgquery.RangeVar, *pgquery.Constraint) {
	a := node.GetAlterTableStmt()
	if a == nil || len(a.GetCmds()) != 1 {
		return nil, nil
	}
	cmd := a.GetCmds()[0].GetAlterTableCmd()
	con := cmd.GetDef().GetConstraint()
	if cmd.GetSubtype() != pgquery.AlterTableType_AT_AddConstraint || con == nil {
		return nil, nil
	}

	return a.GetRelation(), con
}

// adoptedConstraint narrows addedConstraint to the pair a table constraint can carry instead.
func adoptedConstraint(node *pgquery.Node) (*pgquery.RangeVar, *pgquery.Constraint) {
	rel, con := addedConstraint(node)
	if con == nil || con.GetIndexname() == "" || con.GetDeferrable() || con.GetInitdeferred() ||
		(con.GetContype() != pgquery.ConstrType_CONSTR_PRIMARY && con.GetContype() != pgquery.ConstrType_CONSTR_UNIQUE) {
		return nil, nil
	}

	return rel, con
}

// constraintClause is the table constraint that builds the same index inline, false for a shape it cannot.
func constraintClause(ix *pgquery.IndexStmt, con *pgquery.Constraint) (string, bool) {
	if ix == nil || !ix.GetUnique() || ix.GetWhereClause() != nil || ix.GetNullsNotDistinct() ||
		len(ix.GetIndexIncludingParams()) > 0 || len(ix.GetOptions()) > 0 || ix.GetTableSpace() != "" ||
		!strings.EqualFold(ix.GetAccessMethod(), "btree") || len(ix.GetIndexParams()) == 0 {
		return "", false
	}
	cols := make([]string, 0, len(ix.GetIndexParams()))
	for _, p := range ix.GetIndexParams() {
		e := p.GetIndexElem()
		if e.GetName() == "" || len(e.GetCollation()) > 0 || len(e.GetOpclass()) > 0 ||
			e.GetOrdering() != pgquery.SortByDir_SORTBY_DEFAULT ||
			e.GetNullsOrdering() != pgquery.SortByNulls_SORTBY_NULLS_DEFAULT {
			return "", false
		}
		cols = append(cols, pgx.Identifier{e.GetName()}.Sanitize())
	}
	kind := "UNIQUE"
	if con.GetContype() == pgquery.ConstrType_CONSTR_PRIMARY {
		kind = "PRIMARY KEY"
	}

	return fmt.Sprintf("CONSTRAINT %s %s (%s)", pgx.Identifier{con.GetConname()}.Sanitize(), kind, strings.Join(cols, ", ")), true
}

func relKey(rel *pgquery.RangeVar) string {
	return rel.GetSchemaname() + "." + rel.GetRelname()
}
