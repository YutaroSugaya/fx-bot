package repository

import (
	"fmt"
	"strings"
)

// SafeIntegrationTestDSN validates that an integration-test DSN can NEVER point
// at production data. The integration suite empties every table (including
// trades) before each test, so an INTEGRATION_TEST_DB_URL that points at the live DSN
// would wipe the live trade history. The Makefile guard only
// protects `make` invocations; a raw `go test -tags integration` bypasses it.
// This guard lives inside requireDB so it fires no matter how the suite is
// launched, and no matter which wrapper / tool environment is in effect.
//
// Two independent walls, either failing is fatal:
//  1. the DSN's database name must end in "_test" (naming convention);
//  2. it must differ from DATABASE_URL (the live DSN), when that is set.
func SafeIntegrationTestDSN(testDSN, databaseURL string) error {
	if testDSN == "" {
		return fmt.Errorf("INTEGRATION_TEST_DB_URL is empty")
	}
	if databaseURL != "" && testDSN == databaseURL {
		return fmt.Errorf("INTEGRATION_TEST_DB_URL must differ from DATABASE_URL — refusing to run truncating tests against the live DB")
	}
	db := dbNameFromDSN(testDSN)
	if !strings.HasSuffix(db, "_test") {
		// The DSN itself is not echoed: it may carry a password.
		return fmt.Errorf("INTEGRATION_TEST_DB_URL database name %q must end in \"_test\" — refusing to run truncating tests against a non-_test DB", db)
	}
	return nil
}

// dbNameFromDSN extracts the database name from a postgres DSN of the form
// postgres://user:pass@host:port/DBNAME?params. Returns "" if not parseable.
func dbNameFromDSN(dsn string) string {
	s := dsn
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return ""
	}
	s = s[slash+1:]
	if q := strings.IndexByte(s, '?'); q >= 0 {
		s = s[:q]
	}
	return s
}
