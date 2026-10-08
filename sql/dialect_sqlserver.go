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

type sqlServerDialect struct{}

func (sqlServerDialect) Name() string                       { return "sqlserver" }
func (sqlServerDialect) Placeholder(n int) string           { return "@p" + strconv.Itoa(n) }
func (sqlServerDialect) QuoteIdentifier(name string) string { return "[" + name + "]" }
func (d sqlServerDialect) Upsert(table string, rows int) string {
	// HOLDLOCK makes MERGE behave atomically under concurrent writers.
	return fmt.Sprintf("MERGE %s WITH (HOLDLOCK) AS target USING (VALUES %s) AS src (k, v) ON target.%s = src.k "+
		"WHEN MATCHED THEN UPDATE SET %s = src.v "+
		"WHEN NOT MATCHED THEN INSERT (%s, %s) VALUES (src.k, src.v);",
		table, valuesList(d, rows), d.QuoteIdentifier(keyColumn),
		d.QuoteIdentifier(valueColumn),
		d.QuoteIdentifier(keyColumn), d.QuoteIdentifier(valueColumn))
}
func (sqlServerDialect) ByteLength(column string) string { return "DATALENGTH(" + column + ")" }
func (sqlServerDialect) LimitSuffix(rows int) string {
	return "OFFSET 0 ROWS FETCH NEXT " + strconv.Itoa(rows) + " ROWS ONLY"
}
func (d sqlServerDialect) LockRow(ctx context.Context, tx *sql.Tx, table string, key []byte) error {
	// UPDLOCK makes the lock exclusive among writers, HOLDLOCK keeps it until the transaction ends.
	return lockRowBySelect(ctx, tx, fmt.Sprintf("SELECT %s FROM %s WITH (UPDLOCK, HOLDLOCK) WHERE %s = @p1",
		d.QuoteIdentifier(keyColumn), table, d.QuoteIdentifier(keyColumn)), key)
}
func (sqlServerDialect) ReadTxOptions() *sql.TxOptions {
	// SNAPSHOT isolation requires ALLOW_SNAPSHOT_ISOLATION on the database, so stick to the default.
	return nil
}
func (sqlServerDialect) WriteTxOptions() *sql.TxOptions { return nil }
