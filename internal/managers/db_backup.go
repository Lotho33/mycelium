package managers

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// SnapshotTo writes a consistent, compacted copy of the whole database to
// path (which must not exist yet). VACUUM INTO reads one snapshot, so a
// write landing meanwhile is either fully in the copy or not at all, and the
// WAL is folded in: the copy is a single self-contained file.
func (m *DBManager) SnapshotTo(path string) error {
	_, err := m.db.Exec(`VACUUM INTO ?`, path)
	return err
}

// TableCopy is one table to copy from an attached backup database. Where is
// an SQL condition over the backup rows ("" = every row); it may reference
// the backup schema as bk.<table>. Args fill its placeholders.
type TableCopy struct {
	Table string
	Where string
	Args  []any
}

// ImportTables copies rows from the SQLite file at srcPath into the live
// database, all copies in one transaction: either every one applies or none
// does. Rows replace same-key rows (INSERT OR REPLACE); everything else on
// this side stays. Only the columns both schemas have are copied, so a backup
// from an older or newer version still imports: a missing column keeps this
// side's default. A table the backup lacks is skipped. Returns the rows
// copied per table.
func (m *DBManager) ImportTables(ctx context.Context, srcPath string, copies []TableCopy) (map[string]int64, error) {
	conn, err := m.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	// ATTACH is per connection: everything below runs on this one.
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS bk`, "file:"+srcPath+"?mode=ro"); err != nil {
		return nil, fmt.Errorf("apertura del database del backup: %w", err)
	}
	defer conn.ExecContext(context.Background(), `DETACH DATABASE bk`)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() // no-op after Commit

	out := make(map[string]int64, len(copies))
	for _, c := range copies {
		cols, err := commonColumns(ctx, tx, c.Table)
		if err != nil {
			return nil, err
		}
		if len(cols) == 0 {
			continue // table missing on either side
		}
		list := strings.Join(cols, ", ")
		q := fmt.Sprintf(`INSERT OR REPLACE INTO main.%s (%s) SELECT %s FROM bk.%s`, c.Table, list, list, c.Table)
		if c.Where != "" {
			q += " WHERE " + c.Where
		}
		res, err := tx.ExecContext(ctx, q, c.Args...)
		if err != nil {
			return nil, fmt.Errorf("copia di %s: %w", c.Table, err)
		}
		n, _ := res.RowsAffected()
		out[c.Table] += n
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// commonColumns lists the columns table has both in main and in bk, quoted.
// The table name only ever comes from this package's callers (constants), but
// it is still checked against the schema before being formatted into SQL.
func commonColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	read := func(schema string) (map[string]bool, []string, error) {
		rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?, ?)`, table, schema)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		set := map[string]bool{}
		var order []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, nil, err
			}
			set[n] = true
			order = append(order, n)
		}
		return set, order, rows.Err()
	}
	mainCols, order, err := read("main")
	if err != nil {
		return nil, err
	}
	bkCols, _, err := read("bk")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range order {
		if bkCols[c] && mainCols[c] {
			out = append(out, `"`+strings.ReplaceAll(c, `"`, `""`)+`"`)
		}
	}
	return out, nil
}
