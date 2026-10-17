package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"esa/internal/esa"
)

func TestChooseStoreMode(t *testing.T) {
	const db, api = "postgres://esa@db/esa", "http://redmine:3000/esa/api/state"
	cases := []struct {
		name, database, stateURL, importFile string
		ephemeral                            bool
		want                                 storeMode
		err                                  string
	}{
		{name: "a database is required", err: "set ESA_DATABASE_URL"},
		{name: "serve from Postgres", database: db, want: modeServe},
		{name: "serve from an ephemeral schema", database: db, ephemeral: true, want: modeServe},
		{name: "import a JSON file", database: db, importFile: "state.json", want: modeImport},
		{name: "import needs Postgres", importFile: "state.json", err: "set ESA_DATABASE_URL"},
		{name: "import cannot target an ephemeral schema", database: db, importFile: "state.json", ephemeral: true, err: "-ephemeral-schema"},
		{name: "state API is refused", stateURL: api, err: "no longer supported"},
		{name: "state API is refused with Postgres", database: db, stateURL: api, err: "no longer supported"},
		{name: "state API is refused during an import", database: db, stateURL: api, importFile: "state.json", err: "no longer supported"},
	}
	for _, c := range cases {
		got, err := chooseStoreMode(c.database, c.stateURL, c.importFile, c.ephemeral)
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

// An empty database must not hide a JSON store that was never imported.
func TestRefuseUnimportedStore(t *testing.T) {
	legacy := filepath.Join(t.TempDir(), "state.json")
	var empty esa.State
	if err := refuseUnimportedStore(empty, legacy); err != nil {
		t.Fatal("refused without a legacy file", err)
	}
	if err := os.WriteFile(legacy, []byte(`{"schema_version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseUnimportedStore(empty, legacy); err == nil || !strings.Contains(err.Error(), "-import-state "+legacy) {
		t.Fatal("an empty database started over an unimported JSON store", err)
	}
	var imported esa.State
	imported.Tickets.Put("T1", esa.Ticket{ID: "T1"})
	if err := refuseUnimportedStore(imported, legacy); err != nil {
		t.Fatal("refused a database that already holds state", err)
	}
	if err := refuseUnimportedStore(empty, ""); err != nil {
		t.Fatal("refused with the check disabled", err)
	}
}
