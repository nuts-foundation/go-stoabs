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
	"errors"
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
	// LockRow takes an exclusive, transaction-scoped lock on the row with the given key in the given (quoted) lock
	// table, blocking until it is available or ctx expires. It returns ErrLockRowMissing when the row does not exist,
	// because then no lock was taken.
	LockRow(ctx context.Context, tx *sql.Tx, table string, key []byte) error
	// ReadTxOptions returns the options for read-only transactions, or nil for the database default.
	ReadTxOptions() *sql.TxOptions
	// WriteTxOptions returns the options for writable transactions, or nil for the database default.
	WriteTxOptions() *sql.TxOptions
}

// ErrLockRowMissing is returned when the lock table has no row for the lock key, so no write lock could be taken.
// The application creates and seeds the lock table together with the shelf tables.
var ErrLockRowMissing = errors.New("lock row is missing from the lock table")

// lockRowBySelect takes the lock with a locking SELECT and verifies a row was returned.
func lockRowBySelect(ctx context.Context, tx *sql.Tx, query string, key []byte) error {
	var k []byte
	err := tx.QueryRowContext(ctx, query, key).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLockRowMissing
	}
	return err
}

// Column names used in the shelf tables. The application creates the tables, see the package documentation.
const (
	keyColumn   = "key"
	valueColumn = "value"
)

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
