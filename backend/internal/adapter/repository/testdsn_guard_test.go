package repository

import (
	"strings"
	"testing"
)

// The integration suite empties every table it touches, so if
// INTEGRATION_TEST_DB_URL points at the live DB it wipes the LIVE trades.
// The Makefile guard only protects `make` invocations; a raw
// `go test -tags integration` bypasses it. SafeIntegrationTestDSN is the last
// wall, enforced inside requireDB itself — independent of make or any wrapper tooling.
func TestSafeIntegrationTestDSN(t *testing.T) {
	const live = "postgres://fxbot:fxbot@localhost:5432/fxbot?sslmode=disable"
	const test = "postgres://fxbot:fxbot@localhost:5432/fxbot_test?sslmode=disable"
	cases := []struct {
		name    string
		testDSN string
		dbURL   string
		wantErr bool
	}{
		{"empty test dsn", "", live, true},
		{"live db (no _test suffix) rejected", live, "", true},
		{"equals DATABASE_URL rejected", test, test, true},
		{"proper _test db ok", test, live, true == false},
		{"different non-test db rejected", "postgres://u:p@h:5432/prod?sslmode=disable", live, true},
		{"no db name rejected", "postgres://u:p@h:5432/?sslmode=disable", live, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SafeIntegrationTestDSN(tc.testDSN, tc.dbURL)
			if (err != nil) != tc.wantErr {
				t.Errorf("SafeIntegrationTestDSN(%q,%q) err=%v, wantErr=%v", tc.testDSN, tc.dbURL, err, tc.wantErr)
			}
		})
	}
}

// The rejection error is printed to stderr / CI logs, so it must never echo the
// DSN's password.
func TestSafeIntegrationTestDSN_ErrorDoesNotLeakPassword(t *testing.T) {
	const secret = "s3cr3t-pw"
	err := SafeIntegrationTestDSN("postgres://u:"+secret+"@h:5432/prod?sslmode=disable", "")
	if err == nil {
		t.Fatal("want rejection for a non-_test DSN")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error message leaks the DSN password: %v", err)
	}
}

func TestDBNameFromDSN(t *testing.T) {
	cases := map[string]string{
		"postgres://fxbot:fxbot@localhost:5432/fxbot_test?sslmode=disable": "fxbot_test",
		"postgres://fxbot:fxbot@localhost:5432/fxbot":                      "fxbot",
		"postgres://u:p@h:5432/?x=1":                                       "",
		"not-a-dsn":                                                        "",
	}
	for dsn, want := range cases {
		if got := dbNameFromDSN(dsn); got != want {
			t.Errorf("dbNameFromDSN(%q) = %q, want %q", dsn, got, want)
		}
	}
}
