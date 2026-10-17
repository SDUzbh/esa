package main

import (
	"strings"
	"testing"
)

func TestChooseStoreMode(t *testing.T) {
	const db, api = "postgres://esa@db/esa", "http://redmine:3000/esa/api/state"
	cases := []struct {
		name, database, stateURL, importFile, importURL string
		want                                            storeMode
		err                                             string
	}{
		{name: "JSON store by default", want: modeJSON},
		{name: "Postgres store", database: db, want: modePostgres},
		{name: "import a JSON file", database: db, importFile: "state.json", want: modeImport},
		{name: "import the state API while ESA_STATE_URL is still set", database: db, stateURL: api, importURL: api, want: modeImport},
		{name: "state API store is retired", stateURL: api, err: "no longer runs on the Redmine state API"},
		{name: "state API store is retired even with Postgres", database: db, stateURL: api, err: "no longer runs on the Redmine state API"},
		{name: "import needs Postgres", importFile: "state.json", err: "requires -database-url"},
		{name: "import into the state API is retired", stateURL: api, importFile: "state.json", err: "requires -database-url"},
		{name: "one import source", database: db, importFile: "state.json", importURL: api, err: "exactly one"},
	}
	for _, c := range cases {
		got, err := chooseStoreMode(c.database, c.stateURL, c.importFile, c.importURL)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: got %v, %v; want error containing %q", c.name, got, err, c.err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %v, %v; want %v", c.name, got, err, c.want)
		}
	}
}

func TestRetiredStateAPIErrorExplainsMigration(t *testing.T) {
	_, err := chooseStoreMode("", "http://redmine:3000/esa/api/state", "", "")
	for _, part := range []string{"-import-state-url", "ESA_STATE_URL", "postgres-state-store"} {
		if err == nil || !strings.Contains(err.Error(), part) {
			t.Fatalf("error does not explain the migration (%q missing): %v", part, err)
		}
	}
}
