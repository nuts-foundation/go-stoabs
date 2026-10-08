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

type mysqlDialect struct{}

func (mysqlDialect) Name() string                       { return "mysql" }
func (mysqlDialect) Placeholder(_ int) string           { return "?" }
func (mysqlDialect) QuoteIdentifier(name string) string { return "`" + name + "`" }
func (d mysqlDialect) Upsert(table string, rows int) string {
	// VALUES() in ON DUPLICATE KEY UPDATE is deprecated in MySQL 8.0.20+, but the replacement (row alias) is not
	// supported by MariaDB. VALUES() still works on both.
	return fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES %s ON DUPLICATE KEY UPDATE %s = VALUES(%s)",
		table, d.QuoteIdentifier(keyColumn), d.QuoteIdentifier(valueColumn), valuesList(d, rows),
		d.QuoteIdentifier(valueColumn), d.QuoteIdentifier(valueColumn))
}
func (mysqlDialect) ByteLength(column string) string { return "LENGTH(" + column + ")" }
func (mysqlDialect) LimitSuffix(rows int) string     { return "LIMIT " + strconv.Itoa(rows) }
func (d mysqlDialect) LockRow(ctx context.Context, tx *sql.Tx, table string, key []byte) error {
	return lockRowBySelect(ctx, tx, fmt.Sprintf("SELECT %s FROM %s WHERE %s = ? FOR UPDATE",
		d.QuoteIdentifier(keyColumn), table, d.QuoteIdentifier(keyColumn)), key)
}
func (mysqlDialect) ReadTxOptions() *sql.TxOptions {
	// InnoDB's default is REPEATABLE READ, which gives a consistent snapshot per transaction.
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}
func (mysqlDialect) WriteTxOptions() *sql.TxOptions { return nil }
