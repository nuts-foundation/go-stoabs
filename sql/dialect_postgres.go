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

type postgresDialect struct{}

func (postgresDialect) Name() string                       { return "postgres" }
func (postgresDialect) Placeholder(n int) string           { return "$" + strconv.Itoa(n) }
func (postgresDialect) QuoteIdentifier(name string) string { return `"` + name + `"` }
func (d postgresDialect) Upsert(table string, rows int) string {
	return fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES %s ON CONFLICT (%s) DO UPDATE SET %s = EXCLUDED.%s",
		table, d.QuoteIdentifier(keyColumn), d.QuoteIdentifier(valueColumn), valuesList(d, rows),
		d.QuoteIdentifier(keyColumn), d.QuoteIdentifier(valueColumn), d.QuoteIdentifier(valueColumn))
}
func (postgresDialect) ByteLength(column string) string { return "length(" + column + ")" }
func (postgresDialect) LimitSuffix(rows int) string     { return "LIMIT " + strconv.Itoa(rows) }
func (d postgresDialect) LockRow(ctx context.Context, tx *sql.Tx, table string, key []byte) error {
	return lockRowBySelect(ctx, tx, fmt.Sprintf("SELECT %s FROM %s WHERE %s = $1 FOR UPDATE",
		d.QuoteIdentifier(keyColumn), table, d.QuoteIdentifier(keyColumn)), key)
}
func (postgresDialect) ReadTxOptions() *sql.TxOptions {
	// Snapshot semantics for reads, like a bbolt read transaction.
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}
func (postgresDialect) WriteTxOptions() *sql.TxOptions { return nil }
