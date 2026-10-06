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
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// Dialect abstracts the SQL syntax differences between the supported databases.
// The backend only needs placeholders, identifier quoting, an upsert statement, a byte-length function
// and transaction options; everything else is standard SQL.
type Dialect interface {
	// Name returns the name of the dialect, for logging.
	Name() string
	// Placeholder returns the n-th (1-based) statement parameter placeholder.
	Placeholder(n int) string
	// QuoteIdentifier quotes a table or column name.
	QuoteIdentifier(name string) string
	// Upsert returns a statement that inserts or replaces the given number of (key, value) rows in the table.
	// Parameters are ordered key1, value1, key2, value2, ...
	Upsert(table string, rows int) string
	// ByteLength returns the SQL expression that yields the length in bytes of the given (binary) column.
	ByteLength(column string) string
	// LimitSuffix returns the clause, appended after ORDER BY, that limits a query to the given number of rows.
	LimitSuffix(rows int) string
	// ReadTxOptions returns the options for read-only transactions, or nil for the database default.
	ReadTxOptions() *sql.TxOptions
	// WriteTxOptions returns the options for writable transactions, or nil for the database default.
	WriteTxOptions() *sql.TxOptions
}

// Column names used in the shelf tables. The application creates the tables, see the package documentation.
const (
	keyColumn   = "key"
	valueColumn = "value"
)

// SQLite returns the dialect for SQLite.
func SQLite() Dialect { return sqliteDialect{} }

// Postgres returns the dialect for PostgreSQL.
func Postgres() Dialect { return postgresDialect{} }

// MySQL returns the dialect for MySQL and MariaDB.
func MySQL() Dialect { return mysqlDialect{} }

// SQLServer returns the dialect for Microsoft SQL Server and Azure SQL.
func SQLServer() Dialect { return sqlServerDialect{} }

// valuesList builds "(p1, p2), (p3, p4), ..." for the given number of rows.
func valuesList(d Dialect, rows int) string {
	var sb strings.Builder
	n := 1
	for i := 0; i < rows; i++ {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(")
		sb.WriteString(d.Placeholder(n))
		sb.WriteString(", ")
		sb.WriteString(d.Placeholder(n + 1))
		sb.WriteString(")")
		n += 2
	}
	return sb.String()
}

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
func (sqliteDialect) ReadTxOptions() *sql.TxOptions   { return nil }
func (sqliteDialect) WriteTxOptions() *sql.TxOptions  { return nil }

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
func (postgresDialect) ReadTxOptions() *sql.TxOptions {
	// Snapshot semantics for reads, like a bbolt read transaction.
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}
func (postgresDialect) WriteTxOptions() *sql.TxOptions { return nil }

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
func (mysqlDialect) ReadTxOptions() *sql.TxOptions {
	// InnoDB's default is REPEATABLE READ, which gives a consistent snapshot per transaction.
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
}
func (mysqlDialect) WriteTxOptions() *sql.TxOptions { return nil }

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
func (sqlServerDialect) ReadTxOptions() *sql.TxOptions {
	// SNAPSHOT isolation requires ALLOW_SNAPSHOT_ISOLATION on the database, so stick to the default.
	return nil
}
func (sqlServerDialect) WriteTxOptions() *sql.TxOptions { return nil }
