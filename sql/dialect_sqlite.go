/*
 * Copyright (C) 2026 Nuts community
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 *
 */

package sql

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

type sqliteDialect struct{}

func (sqliteDialect) Name() string                       { return "sqlite" }
func (sqliteDialect) Placeholder(_ int) string           { return "?" }
func (sqliteDialect) QuoteIdentifier(name string) string { return `"` + name + `"` }
func (d sqliteDialect) Upsert(table string, rows int) string {
	return fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES %s ON CONFLICT(%s) DO UPDATE SET %s = excluded.%s",
		table, d.QuoteIdentifier(keyColumn), d.QuoteIdentifier(valueColumn), valuesList(d, rows),
		d.QuoteIdentifier(keyColumn), d.QuoteIdentifier(valueColumn), d.QuoteIdentifier(valueColumn))
}
func (sqliteDialect) ByteLength(column string) string { return "length(" + column + ")" }
func (sqliteDialect) LimitSuffix(rows int) string     { return "LIMIT " + strconv.Itoa(rows) }
func (d sqliteDialect) LockRow(ctx context.Context, tx *sql.Tx, table string, key []byte) error {
	// SQLite has no FOR UPDATE. A no-op UPDATE takes the database write lock eagerly (instead of at the first real
	// write), which is as exclusive as SQLite gets: one writer per database.
	result, err := tx.ExecContext(ctx, fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s = ?",
		table, d.QuoteIdentifier(valueColumn), d.QuoteIdentifier(valueColumn), d.QuoteIdentifier(keyColumn)), key)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrLockRowMissing
	}
	return nil
}
func (sqliteDialect) ReadTxOptions() *sql.TxOptions  { return nil }
func (sqliteDialect) WriteTxOptions() *sql.TxOptions { return nil }
