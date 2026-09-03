package engine

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Loss is a drop the target would execute over data it still holds.
type Loss struct {
	Drop
	// Rows for a table, non-null values for a column.
	Rows int64
}

// String renders the loss as the object and what it still holds.
func (l Loss) String() string {
	return fmt.Sprintf("%s %s holds %d row(s)", l.Kind(), l.Drop, l.Rows)
}

// PlanDrops lists every table and column plans remove, in statement order.
func PlanDrops(plans []Plan) map[string][]Drop {
	out := map[string][]Drop{}
	for _, p := range plans {
		for _, st := range p.Statements {
			if len(st.Drops) > 0 {
				out[p.Migration.ID()] = append(out[p.Migration.ID()], st.Drops...)
			}
		}
	}

	return out
}

// DataLoss counts what each drop would destroy on db; objects the target does not have are skipped.
func DataLoss(ctx context.Context, db DB, drops []Drop) ([]Loss, error) {
	out := make([]Loss, 0, len(drops))
	for _, d := range drops {
		rows, held, err := rowsHeld(ctx, db, d)
		if err != nil {
			return nil, err
		}
		if held && rows > 0 {
			out = append(out, Loss{Drop: d, Rows: rows})
		}
	}

	return out, nil
}

func rowsHeld(ctx context.Context, db DB, d Drop) (int64, bool, error) {
	rel, err := relationOID(ctx, db, d)
	if err != nil || rel == "" {
		return 0, false, err
	}
	expr := "*"
	if d.Column != "" {
		var exists bool
		if err := db.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_attribute
			 WHERE attrelid = $1::regclass AND attname = $2 AND attnum > 0 AND NOT attisdropped)`,
			rel, d.Column).Scan(&exists); err != nil {
			return 0, false, fmt.Errorf("check column %s: %w", d, err)
		}
		if !exists {
			return 0, false, nil
		}
		expr = pgx.Identifier{d.Column}.Sanitize()
	}
	var rows int64
	if err := db.QueryRow(ctx, "SELECT count("+expr+") FROM "+rel).Scan(&rows); err != nil {
		return 0, false, fmt.Errorf("count %s: %w", d, err)
	}

	return rows, true, nil
}

// relationOID resolves the drop's table the way the session would, and returns "" when it is not there.
func relationOID(ctx context.Context, db DB, d Drop) (string, error) {
	name := pgx.Identifier{d.Table}.Sanitize()
	if d.Schema != "" {
		name = pgx.Identifier{d.Schema, d.Table}.Sanitize()
	}
	var rel *string
	if err := db.QueryRow(ctx, `SELECT to_regclass($1)::text`, name).Scan(&rel); err != nil {
		return "", fmt.Errorf("resolve %s: %w", d, err)
	}
	if rel == nil {
		return "", nil
	}

	return *rel, nil
}
