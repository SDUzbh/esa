package main

import (
	"strings"
	"testing"
)

func TestChooseStoreMode(t *testing.T) {
	const db, api = "postgres://esa@db/esa", "http://redmine:3000/esa/api/state"
	cases := []struct {
		name, database, stateURL, importFile string
		want                                 storeMode
		err                                  string
	}{
		{name: "JSON store by default", want: modeJSON},
		{name: "Postgres store", database: db, want: modePostgres},
		{name: "import a JSON file", database: db, importFile: "state.json", want: modeImport},
		{name: "import needs Postgres", importFile: "state.json", err: "requires -database-url"},
		{name: "state API is refused", stateURL: api, err: "no longer supported"},
		{name: "state API is refused with Postgres", database: db, stateURL: api, err: "no longer supported"},
		{name: "state API is refused during an import", database: db, stateURL: api, importFile: "state.json", err: "no longer supported"},
	}
	for _, c := range cases {
		got, err := chooseStoreMode(c.database, c.stateURL, c.importFile)
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
