package engine

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
)

// AdoptedTable is the bookkeeping table one other migration tool keeps its own state in.
type AdoptedTable struct {
	Tool     string
	Name     string
	Required []string
	Known    []string
}

// AdoptedTables is the closed list of bookkeeping tables godwit recognises, one per tool it can be adopted from.
var AdoptedTables = []AdoptedTable{
	{
		Tool: "golang-migrate", Name: "schema_migrations",
		Required: []string{"version", "dirty"}, Known: []string{"version", "dirty"},
	},
	{
		Tool: "rails", Name: "schema_migrations",
		Required: []string{"version"}, Known: []string{"version"},
	},
	{
		Tool: "rails", Name: "ar_internal_metadata",
		Required: []string{"key", "value"}, Known: []string{"key", "value", "created_at", "updated_at"},
	},
	{
		Tool: "flyway", Name: "flyway_schema_history",
		Required: []string{"installed_rank", "version", "script", "checksum", "success"},
		Known: []string{
			"installed_rank", "version", "description", "type", "script", "checksum",
			"installed_by", "installed_on", "execution_time", "success",
		},
	},
	{
		Tool: "liquibase", Name: "databasechangelog",
		Required: []string{"id", "author", "filename", "dateexecuted", "orderexecuted", "md5sum"},
		Known: []string{
			"id", "author", "filename", "dateexecuted", "orderexecuted", "exectype", "md5sum",
			"description", "comments", "tag", "liquibase", "contexts", "labels", "deployment_id",
		},
	},
	{
		Tool: "liquibase", Name: "databasechangeloglock",
		Required: []string{"id", "locked"}, Known: []string{"id", "locked", "lockgranted", "lockedby"},
	},
	{
		Tool: "alembic", Name: "alembic_version",
		Required: []string{"version_num"}, Known: []string{"version_num"},
	},
	{
		Tool: "prisma", Name: "_prisma_migrations",
		Required: []string{"id", "checksum", "migration_name", "started_at"},
		Known: []string{
			"id", "checksum", "finished_at", "migration_name", "logs", "rolled_back_at",
			"started_at", "applied_steps_count",
		},
	},
	{
		Tool: "atlas", Name: "atlas_schema_revisions",
		Required: []string{"version", "applied", "total", "hash"},
		Known: []string{
			"version", "description", "type", "applied", "total", "executed_at", "execution_time",
			"error", "error_stmt", "hash", "partial_hashes", "operator_version",
		},
	},
}

// Adopted is one bookkeeping table found on a database, with the tool it belongs to.
type Adopted struct {
	Tool   string `json:"tool"`
	Schema string `json:"schema"`
	Table  string `json:"table"`
}

// Qualified is the table as the schema snapshot names it.
func (a Adopted) Qualified() string {
	return a.Schema + "." + a.Table
}

// String names the table and what left it behind, as reports render it.
func (a Adopted) String() string {
	return a.Qualified() + " (" + a.Tool + ")"
}

// DetectAdopted lists the bookkeeping tables of other migration tools this database carries, matched by columns.
func DetectAdopted(ctx context.Context, db DB) ([]Adopted, error) {
	names := make([]string, 0, len(AdoptedTables))
	for _, t := range AdoptedTables {
		if !slices.Contains(names, t.Name) {
			names = append(names, t.Name)
		}
	}
	rows, err := db.Query(ctx, `
		SELECT c.table_schema, c.table_name, array_agg(c.column_name::text ORDER BY c.column_name)
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name AND t.table_type = 'BASE TABLE'
		WHERE c.table_schema NOT IN ('pg_catalog', 'information_schema') AND c.table_name = ANY($1)
		GROUP BY c.table_schema, c.table_name`, names)
	if err != nil {
		return nil, fmt.Errorf("inspect adopted tables: %w", err)
	}
	var out []Adopted
	var schema, table string
	var columns []string
	if _, err := pgx.ForEachRow(rows, []any{&schema, &table, &columns}, func() error {
		if tool, ok := matchAdopted(table, columns); ok {
			out = append(out, Adopted{Tool: tool, Schema: schema, Table: table})
		}

		return nil
	}); err != nil {
		return nil, fmt.Errorf("read adopted tables: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Qualified() < out[j].Qualified() })

	return out, nil
}

func matchAdopted(table string, columns []string) (string, bool) {
	for _, t := range AdoptedTables {
		if t.Name == table && covers(t.Known, columns) && covers(columns, t.Required) {
			return t.Tool, true
		}
	}

	return "", false
}

func covers(outer, inner []string) bool {
	for _, s := range inner {
		if !slices.Contains(outer, s) {
			return false
		}
	}

	return true
}

// AdoptedLines renders the ignored tables for a report, one entry each.
func AdoptedLines(as []Adopted) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, a.String())
	}

	return out
}
