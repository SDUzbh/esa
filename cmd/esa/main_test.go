package main

import (
	"strings"
	"testing"
)

func TestCheckStoreFlags(t *testing.T) {
	const db, api = "postgres://esa@db/esa", "http://redmine:3000/esa/api/state"
	cases := []struct{ name, database, stateURL, err string }{
		{name: "a database is required", err: "set ESA_DATABASE_URL"},
		{name: "Postgres", database: db},
		{name: "state API is refused", stateURL: api, err: "no longer supported"},
		{name: "state API is refused with Postgres", database: db, stateURL: api, err: "no longer supported"},
	}
	for _, c := range cases {
		err := checkStoreFlags(c.database, c.stateURL)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: got %v, want error containing %q", c.name, err, c.err)
		}
	}
}
