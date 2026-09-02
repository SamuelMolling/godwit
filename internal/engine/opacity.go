package engine

import (
	pgquery "github.com/pganalyze/pg_query_go/v6"
)

// Reasons a statement's effect cannot be read back from a schema snapshot.
const (
	OpaqueDML     = "has DML, must execute"
	OpaqueUnknown = "effect not inspectable"
)

func opacity(node *pgquery.Node) string {
	if isDML(node) {
		return OpaqueDML
	}
	var visible bool
	switch {
	case node.GetCreateStmt() != nil:
		visible = plainCreate(node.GetCreateStmt())
	case node.GetIndexStmt() != nil:
		visible = node.GetIndexStmt().TableSpace == ""
	case node.GetViewStmt() != nil:
		v := node.GetViewStmt()
		visible = len(v.Options) == 0 && v.WithCheckOption == pgquery.ViewCheckOption_NO_CHECK_OPTION
	case node.GetDropStmt() != nil:
		visible = visibleDrop(node.GetDropStmt().RemoveType)
	case node.GetRenameStmt() != nil:
		visible = visibleRename(node.GetRenameStmt())
	case node.GetAlterTableStmt() != nil:
		visible = visibleAlterTable(node.GetAlterTableStmt())
	}
	if visible {
		return ""
	}

	return OpaqueUnknown
}

func isDML(node *pgquery.Node) bool {
	return node.GetInsertStmt() != nil || node.GetUpdateStmt() != nil || node.GetDeleteStmt() != nil ||
		node.GetMergeStmt() != nil || node.GetSelectStmt() != nil || node.GetCopyStmt() != nil ||
		node.GetTruncateStmt() != nil || node.GetCallStmt() != nil || node.GetDoStmt() != nil
}

func plainCreate(c *pgquery.CreateStmt) bool {
	if c.Relation.Relpersistence != "p" || len(c.Options) > 0 || c.Tablespacename != "" || c.AccessMethod != "" ||
		c.Partbound != nil || c.Partspec != nil || len(c.InhRelations) > 0 || c.OfTypename != nil {
		return false
	}
	for _, elt := range c.TableElts {
		if elt.GetTableLikeClause() != nil {
			return false
		}
		if col := elt.GetColumnDef(); col != nil && !plainColumn(col) {
			return false
		}
	}

	return true
}

func plainColumn(col *pgquery.ColumnDef) bool {
	if col.CollClause != nil {
		return false
	}
	for _, cn := range col.Constraints {
		switch cn.GetConstraint().Contype {
		case pgquery.ConstrType_CONSTR_IDENTITY, pgquery.ConstrType_CONSTR_GENERATED:
			return false
		default:
		}
	}

	return true
}

func visibleDrop(t pgquery.ObjectType) bool {
	switch t {
	case pgquery.ObjectType_OBJECT_TABLE, pgquery.ObjectType_OBJECT_INDEX, pgquery.ObjectType_OBJECT_VIEW:
		return true
	default:
		return false
	}
}

func visibleRename(r *pgquery.RenameStmt) bool {
	switch r.RenameType {
	case pgquery.ObjectType_OBJECT_TABLE, pgquery.ObjectType_OBJECT_INDEX, pgquery.ObjectType_OBJECT_VIEW,
		pgquery.ObjectType_OBJECT_TABCONSTRAINT:
		return true
	case pgquery.ObjectType_OBJECT_COLUMN:
		return r.RelationType == pgquery.ObjectType_OBJECT_TABLE
	default:
		return false
	}
}

func visibleAlterTable(a *pgquery.AlterTableStmt) bool {
	if a.Objtype != pgquery.ObjectType_OBJECT_TABLE {
		return false
	}
	for _, cmdNode := range a.Cmds {
		cmd := cmdNode.GetAlterTableCmd()
		switch cmd.Subtype {
		case pgquery.AlterTableType_AT_AddColumn, pgquery.AlterTableType_AT_AlterColumnType:
			if col := cmd.GetDef().GetColumnDef(); col != nil && !plainColumn(col) {
				return false
			}
		case pgquery.AlterTableType_AT_DropColumn, pgquery.AlterTableType_AT_AddConstraint,
			pgquery.AlterTableType_AT_DropConstraint, pgquery.AlterTableType_AT_SetNotNull,
			pgquery.AlterTableType_AT_DropNotNull, pgquery.AlterTableType_AT_ColumnDefault,
			pgquery.AlterTableType_AT_ValidateConstraint:
		default:
			return false
		}
	}

	return true
}
