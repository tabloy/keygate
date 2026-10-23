package store

import (
	"database/sql"
	"errors"

	"github.com/uptrace/bun/driver/pgdriver"
)

// isUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). Uses pgdriver.Error for type-safe inspection
// rather than substring matching on the human-readable message.
//
// Driver-specific: relies on uptrace/bun pgdriver. If the project ever
// migrates to bun's pgxdialect (or another driver), this check must be
// revisited.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr pgdriver.Error
	if errors.As(err, &pgErr) {
		return pgErr.Field('C') == "23505"
	}
	return false
}

// isLockTimeout reports whether err is a wait that lock_timeout ended
// (SQLSTATE 55P03).
func isLockTimeout(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "55P03"
}

// IsUniqueViolation is isUniqueViolation for handlers that turn a
// duplicate into a 409 instead of a 500.
func IsUniqueViolation(err error) bool { return isUniqueViolation(err) }

// IsForeignKeyViolation reports whether err is a Postgres foreign-key
// violation (SQLSTATE 23503): a row that names a parent which does not
// exist, or a delete of a parent that still has children.
func IsForeignKeyViolation(err error) bool {
	var pgErr pgdriver.Error
	return errors.As(err, &pgErr) && pgErr.Field('C') == "23503"
}

// deletedOne turns a delete that matched no row into sql.ErrNoRows, so a
// handler can answer 404 for an id that does not exist instead of 204.
func deletedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
