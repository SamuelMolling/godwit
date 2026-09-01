package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// Direction selects which side of a migration to plan.
type Direction string

// Migration directions.
const (
	DirectionUp   Direction = "up"
	DirectionDown Direction = "down"
)

// VerifierKind names the recovery strategy for a non-transactional statement.
type VerifierKind string

// Verifier kinds.
const (
	VerifierNone                    VerifierKind = ""
	VerifierCreateIndexConcurrently VerifierKind = "create_index_concurrently"
	VerifierDropIndexConcurrently   VerifierKind = "drop_index_concurrently"
	VerifierRerun                   VerifierKind = "rerun"
)

// Hazard flags a statement that can hurt a live database.
type Hazard struct {
	Code   string
	Detail string
}

// Statement is one classified SQL statement of a plan.
type Statement struct {
	SQL         string
	Hash        string
	NoTx        bool
	Verifier    VerifierKind
	IndexSchema string
	IndexName   string
	Hazards     []Hazard
}

// Plan is the executable form of one migration direction.
type Plan struct {
	Migration  Migration
	Direction  Direction
	Statements []Statement
}

// BuildPlan parses one side of a migration into classified statements.
func BuildPlan(m Migration, dir Direction) (Plan, error) {
	sql := m.UpSQL
	if dir == DirectionDown {
		sql = m.DownSQL
	}

	res, err := pgquery.Parse(sql)
	if err != nil {
		return Plan{}, fmt.Errorf("%d_%s (%s): parse: %w", m.Version, m.Name, dir, err)
	}
	if len(res.Stmts) == 0 {
		return Plan{}, fmt.Errorf("%d_%s (%s): no statements", m.Version, m.Name, dir)
	}

	p := Plan{Migration: m, Direction: dir}
	for _, raw := range res.Stmts {
		text := stmtText(sql, raw)
		st := Statement{SQL: text, Hash: hashSQL(text)}
		if err := classify(raw.Stmt, &st); err != nil {
			return Plan{}, fmt.Errorf("%d_%s (%s): %w", m.Version, m.Name, dir, err)
		}
		p.Statements = append(p.Statements, st)
	}

	return p, nil
}

func stmtText(sql string, raw *pgquery.RawStmt) string {
	start := int(raw.StmtLocation)
	end := len(sql)
	if raw.StmtLen > 0 {
		end = start + int(raw.StmtLen)
	}
	return strings.TrimSpace(sql[start:end])
}

func hashSQL(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func classify(node *pgquery.Node, st *Statement) error {
	switch {
	case node.GetIndexStmt() != nil:
		return classifyIndex(node.GetIndexStmt(), st)
	case node.GetDropStmt() != nil:
		classifyDrop(node.GetDropStmt(), st)
	case node.GetAlterTableStmt() != nil:
		classifyAlterTable(node.GetAlterTableStmt(), st)
	case node.GetVacuumStmt() != nil:
		st.NoTx = true
		st.Verifier = VerifierRerun
	case node.GetRefreshMatViewStmt() != nil:
		if node.GetRefreshMatViewStmt().Concurrent {
			st.NoTx = true
			st.Verifier = VerifierRerun
		}
	case node.GetReindexStmt() != nil:
		if reindexConcurrent(node.GetReindexStmt()) {
			st.NoTx = true
			st.Verifier = VerifierRerun
		}
	}

	return nil
}

func classifyIndex(idx *pgquery.IndexStmt, st *Statement) error {
	if !idx.Concurrent {
		st.Hazards = append(st.Hazards, Hazard{
			Code:   "H001",
			Detail: "CREATE INDEX without CONCURRENTLY blocks writes on " + idx.Relation.Relname,
		})

		return nil
	}
	if idx.Idxname == "" {
		return fmt.Errorf("CREATE INDEX CONCURRENTLY must name the index so godwit can verify it after a crash")
	}
	st.NoTx = true
	st.Verifier = VerifierCreateIndexConcurrently
	st.IndexSchema = idx.Relation.Schemaname
	st.IndexName = idx.Idxname

	return nil
}

func classifyDrop(d *pgquery.DropStmt, st *Statement) {
	switch d.RemoveType {
	case pgquery.ObjectType_OBJECT_TABLE:
		st.Hazards = append(st.Hazards, Hazard{Code: "H002", Detail: "DROP TABLE is destructive"})
	case pgquery.ObjectType_OBJECT_INDEX:
		if d.Concurrent {
			st.NoTx = true
			st.Verifier = VerifierDropIndexConcurrently
			st.IndexSchema, st.IndexName = qualifiedName(d.Objects)
		}
	default:
	}
}

func qualifiedName(objects []*pgquery.Node) (schema, name string) {
	items := objects[0].GetList().GetItems()
	parts := make([]string, 0, len(items))
	for _, it := range items {
		parts = append(parts, it.GetString_().GetSval())
	}
	if len(parts) == 1 {
		return "", parts[0]
	}

	return parts[len(parts)-2], parts[len(parts)-1]
}

func classifyAlterTable(a *pgquery.AlterTableStmt, st *Statement) {
	for _, cmdNode := range a.Cmds {
		cmd := cmdNode.GetAlterTableCmd()
		switch cmd.Subtype {
		case pgquery.AlterTableType_AT_DropColumn:
			st.Hazards = append(st.Hazards, Hazard{Code: "H003", Detail: "DROP COLUMN is destructive"})
		case pgquery.AlterTableType_AT_AlterColumnType:
			st.Hazards = append(st.Hazards, Hazard{Code: "H004", Detail: "ALTER COLUMN TYPE rewrites the table under an exclusive lock"})
		case pgquery.AlterTableType_AT_AddColumn:
			if col := cmd.GetDef().GetColumnDef(); col != nil && notNullWithoutDefault(col) {
				st.Hazards = append(st.Hazards, Hazard{Code: "H005", Detail: "ADD COLUMN NOT NULL without DEFAULT fails on non-empty tables"})
			}
		default:
		}
	}
}

func notNullWithoutDefault(col *pgquery.ColumnDef) bool {
	notNull, hasDefault := false, false
	for _, cn := range col.Constraints {
		switch cn.GetConstraint().Contype {
		case pgquery.ConstrType_CONSTR_NOTNULL:
			notNull = true
		case pgquery.ConstrType_CONSTR_DEFAULT:
			hasDefault = true
		default:
		}
	}

	return notNull && !hasDefault
}

func reindexConcurrent(r *pgquery.ReindexStmt) bool {
	for _, p := range r.Params {
		if strings.EqualFold(p.GetDefElem().GetDefname(), "concurrently") {
			return true
		}
	}

	return false
}
