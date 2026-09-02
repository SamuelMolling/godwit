package engine

import (
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/proto"
)

var deparseSQL = pgquery.Deparse

func deparse(node *pgquery.Node) string {
	out, err := deparseSQL(&pgquery.ParseResult{Stmts: []*pgquery.RawStmt{{Stmt: node}}})
	if err != nil {
		return "/* " + err.Error() + " */"
	}

	return out
}

func statements(nodes ...*pgquery.Node) []string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, deparse(n)+";")
	}

	return out
}

func recipe(lines ...string) string {
	return strings.Join(lines, "\n")
}

func ident(name string) string {
	sel := &pgquery.SelectStmt{TargetList: []*pgquery.Node{pgquery.MakeResTargetNodeWithVal(columnRef(name), 0)}}

	return strings.TrimPrefix(deparse(&pgquery.Node{Node: &pgquery.Node_SelectStmt{SelectStmt: sel}}), "SELECT ")
}

func exprText(expr *pgquery.Node) string {
	sel := &pgquery.SelectStmt{TargetList: []*pgquery.Node{pgquery.MakeResTargetNodeWithVal(expr, 0)}}

	return strings.TrimPrefix(deparse(&pgquery.Node{Node: &pgquery.Node_SelectStmt{SelectStmt: sel}}), "SELECT ")
}

func columnRef(fields ...string) *pgquery.Node {
	nodes := make([]*pgquery.Node, 0, len(fields))
	for _, f := range fields {
		nodes = append(nodes, pgquery.MakeStrNode(f))
	}

	return pgquery.MakeColumnRefNode(nodes, 0)
}

func qualified(rel *pgquery.RangeVar, name string) []*pgquery.Node {
	if rel.Schemaname == "" {
		return []*pgquery.Node{pgquery.MakeStrNode(name)}
	}

	return []*pgquery.Node{pgquery.MakeStrNode(rel.Schemaname), pgquery.MakeStrNode(name)}
}

func alterTable(rel *pgquery.RangeVar, cmds ...*pgquery.AlterTableCmd) *pgquery.Node {
	nodes := make([]*pgquery.Node, 0, len(cmds))
	for _, c := range cmds {
		nodes = append(nodes, &pgquery.Node{Node: &pgquery.Node_AlterTableCmd{AlterTableCmd: c}})
	}

	return &pgquery.Node{Node: &pgquery.Node_AlterTableStmt{AlterTableStmt: &pgquery.AlterTableStmt{
		Relation: rel, Objtype: pgquery.ObjectType_OBJECT_TABLE, Cmds: nodes,
	}}}
}

func addColumn(rel *pgquery.RangeVar, col *pgquery.ColumnDef) *pgquery.Node {
	return alterTable(rel, &pgquery.AlterTableCmd{
		Subtype: pgquery.AlterTableType_AT_AddColumn, Def: &pgquery.Node{Node: &pgquery.Node_ColumnDef{ColumnDef: col}},
	})
}

func addConstraint(rel *pgquery.RangeVar, cn *pgquery.Constraint) *pgquery.Node {
	return alterTable(rel, &pgquery.AlterTableCmd{
		Subtype: pgquery.AlterTableType_AT_AddConstraint, Def: &pgquery.Node{Node: &pgquery.Node_Constraint{Constraint: cn}},
	})
}

func namedCmd(rel *pgquery.RangeVar, subtype pgquery.AlterTableType, name string) *pgquery.Node {
	return alterTable(rel, &pgquery.AlterTableCmd{Subtype: subtype, Name: name})
}

func indexNode(idx *pgquery.IndexStmt) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_IndexStmt{IndexStmt: idx}}
}

func dropNode(d *pgquery.DropStmt) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_DropStmt{DropStmt: d}}
}

func joinNames(nodes []*pgquery.Node) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, n.GetString_().GetSval())
	}

	return strings.Join(parts, "_")
}

func indexColumns(params []*pgquery.Node) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		name := p.GetIndexElem().GetName()
		if name == "" {
			name = "expr"
		}
		parts = append(parts, name)
	}

	return strings.Join(parts, "_")
}

func recipeIndex(idx *pgquery.IndexStmt) string {
	c := proto.Clone(idx).(*pgquery.IndexStmt)
	c.Concurrent = true
	if c.Idxname == "" {
		c.Idxname = idx.Relation.Relname + "_" + indexColumns(idx.IndexParams) + "_idx"
	}

	return recipe(statements(indexNode(c))...)
}

func recipeDropIndex(d *pgquery.DropStmt) string {
	nodes := make([]*pgquery.Node, 0, len(d.Objects))
	for _, obj := range d.Objects {
		nodes = append(nodes, dropNode(&pgquery.DropStmt{
			RemoveType: pgquery.ObjectType_OBJECT_INDEX, Concurrent: true, MissingOk: d.MissingOk, Behavior: d.Behavior, Objects: []*pgquery.Node{obj},
		}))
	}

	return recipe(statements(nodes...)...)
}

func recipeAlterType(rel *pgquery.RangeVar, cmd *pgquery.AlterTableCmd) string {
	col, newCol, oldCol := cmd.Name, cmd.Name+"_new", cmd.Name+"_old"
	def := cmd.GetDef().GetColumnDef()
	expr := def.RawDefault
	if expr == nil {
		expr = &pgquery.Node{Node: &pgquery.Node_TypeCast{TypeCast: &pgquery.TypeCast{Arg: columnRef(col), TypeName: def.TypeName}}}
	}
	sync := rel.Relname + "_" + col + "_sync"
	body := " BEGIN SELECT " + exprText(expr) + " INTO new." + ident(newCol) + " FROM (SELECT new.*) AS " + ident(rel.Relname) + "; RETURN new; END "
	lines := []string{"-- 1. expand: add the new column and keep both in sync"}
	lines = append(lines, statements(
		addColumn(rel, &pgquery.ColumnDef{Colname: newCol, TypeName: def.TypeName, CollClause: def.CollClause}),
		&pgquery.Node{Node: &pgquery.Node_CreateFunctionStmt{CreateFunctionStmt: &pgquery.CreateFunctionStmt{
			Funcname:   qualified(rel, sync),
			ReturnType: &pgquery.TypeName{Names: []*pgquery.Node{pgquery.MakeStrNode("trigger")}, Typemod: -1},
			Options: []*pgquery.Node{
				pgquery.MakeSimpleDefElemNode("language", pgquery.MakeStrNode("plpgsql"), 0),
				pgquery.MakeSimpleDefElemNode("as", pgquery.MakeListNode([]*pgquery.Node{pgquery.MakeStrNode(body)}), 0),
			},
		}}},
		&pgquery.Node{Node: &pgquery.Node_CreateTrigStmt{CreateTrigStmt: &pgquery.CreateTrigStmt{
			Trigname: sync, Relation: rel, Funcname: qualified(rel, sync), Row: true, Timing: triggerBefore, Events: triggerInsertOrUpdate,
		}}},
	)...)
	lines = append(lines, "-- 2. backfill in batches outside a migration (replace id with the primary key)")
	lines = append(lines, statements(&pgquery.Node{Node: &pgquery.Node_UpdateStmt{UpdateStmt: &pgquery.UpdateStmt{
		Relation:   rel,
		TargetList: []*pgquery.Node{pgquery.MakeResTargetNodeWithNameAndVal(newCol, expr, 0)},
		WhereClause: pgquery.MakeAExprNode(pgquery.A_Expr_Kind_AEXPR_BETWEEN, []*pgquery.Node{pgquery.MakeStrNode("BETWEEN")}, columnRef("id"),
			pgquery.MakeListNode([]*pgquery.Node{pgquery.MakeParamRefNode(1, 0), pgquery.MakeParamRefNode(2, 0)}), 0),
	}}})...)
	lines = append(lines, "-- 3. later migration: swap the columns")
	lines = append(lines, statements(
		dropNode(&pgquery.DropStmt{RemoveType: pgquery.ObjectType_OBJECT_TRIGGER, Behavior: pgquery.DropBehavior_DROP_RESTRICT, Objects: []*pgquery.Node{
			pgquery.MakeListNode(append(qualified(rel, rel.Relname), pgquery.MakeStrNode(sync))),
		}}),
		dropNode(&pgquery.DropStmt{RemoveType: pgquery.ObjectType_OBJECT_FUNCTION, Behavior: pgquery.DropBehavior_DROP_RESTRICT, Objects: []*pgquery.Node{
			{Node: &pgquery.Node_ObjectWithArgs{ObjectWithArgs: &pgquery.ObjectWithArgs{Objname: qualified(rel, sync)}}},
		}}),
		renameColumn(rel, col, oldCol),
		renameColumn(rel, newCol, col),
	)...)
	lines = append(lines, "-- 4. contract migration (rollout: expand-contract)")
	lines = append(lines, statements(namedCmd(rel, pgquery.AlterTableType_AT_DropColumn, oldCol))...)

	return recipe(lines...)
}

const (
	triggerBefore         = 2
	triggerInsertOrUpdate = 4 | 16
)

func renameColumn(rel *pgquery.RangeVar, from, to string) *pgquery.Node {
	return &pgquery.Node{Node: &pgquery.Node_RenameStmt{RenameStmt: &pgquery.RenameStmt{
		RenameType: pgquery.ObjectType_OBJECT_COLUMN, RelationType: pgquery.ObjectType_OBJECT_TABLE,
		Relation: rel, Subname: from, Newname: to, Behavior: pgquery.DropBehavior_DROP_RESTRICT,
	}}}
}

func recipeAddColumn(rel *pgquery.RangeVar, col *pgquery.ColumnDef) string {
	nullable := proto.Clone(col).(*pgquery.ColumnDef)
	nullable.Constraints = nil
	for _, cn := range col.Constraints {
		if cn.GetConstraint().Contype != pgquery.ConstrType_CONSTR_NOTNULL {
			nullable.Constraints = append(nullable.Constraints, cn)
		}
	}
	defaulted := proto.Clone(col).(*pgquery.ColumnDef)
	defaulted.Constraints = append(defaulted.Constraints, pgquery.MakeDefaultConstraintNode(pgquery.MakeParamRefNode(1, 0), 0))
	lines := []string{"-- 1. add the column nullable, backfill it, then constrain it without a full scan"}
	lines = append(lines, statements(addColumn(rel, nullable))...)
	lines = append(lines, notNullStatements(rel, col.Colname)...)
	lines = append(lines, "-- 2. or, when a constant default fits every existing row, one metadata-only step (PostgreSQL 11+; replace $1 with the constant)")
	lines = append(lines, statements(addColumn(rel, defaulted))...)

	return recipe(lines...)
}

func recipeNotNull(rel *pgquery.RangeVar, column string) string {
	return recipe(notNullStatements(rel, column)...)
}

func notNullStatements(rel *pgquery.RangeVar, column string) []string {
	name := column + "_not_null"
	check := &pgquery.Constraint{
		Contype: pgquery.ConstrType_CONSTR_CHECK, Conname: name, SkipValidation: true,
		RawExpr: &pgquery.Node{Node: &pgquery.Node_NullTest{NullTest: &pgquery.NullTest{Arg: columnRef(column), Nulltesttype: pgquery.NullTestType_IS_NOT_NULL}}},
	}

	return statements(
		addConstraint(rel, check),
		namedCmd(rel, pgquery.AlterTableType_AT_ValidateConstraint, name),
		namedCmd(rel, pgquery.AlterTableType_AT_SetNotNull, column),
		namedCmd(rel, pgquery.AlterTableType_AT_DropConstraint, name),
	)
}

func recipeConstraint(rel *pgquery.RangeVar, cn *pgquery.Constraint) string {
	c := proto.Clone(cn).(*pgquery.Constraint)
	c.SkipValidation = true
	c.InitiallyValid = false
	if c.Conname == "" {
		c.Conname = constraintName(rel, cn)
	}

	return recipe(statements(addConstraint(rel, c), namedCmd(rel, pgquery.AlterTableType_AT_ValidateConstraint, c.Conname))...)
}

func constraintName(rel *pgquery.RangeVar, cn *pgquery.Constraint) string {
	switch cn.Contype {
	case pgquery.ConstrType_CONSTR_FOREIGN:
		return rel.Relname + "_" + joinNames(cn.FkAttrs) + "_fkey"
	case pgquery.ConstrType_CONSTR_CHECK:
		return rel.Relname + "_check"
	case pgquery.ConstrType_CONSTR_PRIMARY:
		return rel.Relname + "_pkey"
	default:
		return rel.Relname + "_" + joinNames(cn.Keys) + "_key"
	}
}

func recipeUsingIndex(rel *pgquery.RangeVar, cn *pgquery.Constraint) string {
	name := cn.Conname
	if name == "" {
		name = constraintName(rel, cn)
	}
	params := make([]*pgquery.Node, 0, len(cn.Keys))
	for _, k := range cn.Keys {
		params = append(params, &pgquery.Node{Node: &pgquery.Node_IndexElem{IndexElem: &pgquery.IndexElem{Name: k.GetString_().GetSval()}}})
	}
	idx := &pgquery.IndexStmt{Idxname: name + "_idx", Relation: rel, AccessMethod: "btree", IndexParams: params, Unique: true, Concurrent: true}
	c := proto.Clone(cn).(*pgquery.Constraint)
	c.Conname, c.Indexname, c.Keys = name, idx.Idxname, nil

	return recipe(statements(indexNode(idx), addConstraint(rel, c))...)
}

func recipeDropTable(d *pgquery.DropStmt) string {
	names := make([]string, 0, len(d.Objects))
	for _, obj := range d.Objects {
		schema, name := qualifiedName([]*pgquery.Node{obj})
		if schema != "" {
			name = schema + "." + name
		}
		names = append(names, name)
	}

	return "-- expand then contract: ship the application version that no longer uses " + strings.Join(names, ", ") +
		", then run this DROP TABLE as a contract migration (rollout: expand-contract)"
}

func recipeDropColumn(rel *pgquery.RangeVar, column string) string {
	return "-- expand then contract: ship the application version that no longer reads or writes " + rel.Relname + "." + column +
		", then run this DROP COLUMN as a contract migration (rollout: expand-contract)"
}

func recipeRenameTable(r *pgquery.RenameStmt) string {
	return "-- expand then contract: create " + r.Newname + " (CREATE TABLE ... (LIKE " + r.Relation.Relname + " INCLUDING ALL)), " +
		"dual-write and copy the rows, ship the application version that uses " + r.Newname +
		", then DROP TABLE " + r.Relation.Relname + " in a contract migration (rollout: expand-contract)"
}

func recipeRenameColumn(r *pgquery.RenameStmt) string {
	return "-- expand then contract: ALTER TABLE " + r.Relation.Relname + " ADD COLUMN " + r.Newname + " with the type of " + r.Subname +
		", backfill and keep both in sync (see the H004 recipe), ship the application version that uses " + r.Newname +
		", then DROP COLUMN " + r.Subname + " in a contract migration (rollout: expand-contract)"
}
