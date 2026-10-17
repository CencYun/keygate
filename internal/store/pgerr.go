package store

import (
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
