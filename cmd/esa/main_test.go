package main

import (
	"strings"
	"testing"
)

func TestHealthURLFollowsListen(t *testing.T) {
	for listen, want := range map[string]string{
		"127.0.0.1:8090": "http://127.0.0.1:8090/healthz?role=active",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz?role=active",
		":8091":          "http://127.0.0.1:8091/healthz?role=active",
		"[::]:8092":      "http://[::1]:8092/healthz?role=active",
		"esa.local:8093": "http://esa.local:8093/healthz?role=active",
	} {
		if got, err := healthURL(listen); err != nil || got != want {
			t.Errorf("healthURL(%q) = %q, %v; want %q", listen, got, err, want)
		}
	}
	if _, err := healthURL("8090"); err == nil {
		t.Error("a listen address without a port was accepted")
	}
}

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

// Review Issue 5: one secret per role; reusing one is refused at startup.
func TestCheckTokensRequiresOneSecretPerRole(t *testing.T) {
	api, operator, analyzer := strings.Repeat("a", 24), strings.Repeat("o", 24), strings.Repeat("z", 24)
	for name, tc := range map[string]struct {
		api, operator, analyzer string
		configured, ok          bool
	}{
		"all distinct":                  {api, operator, analyzer, true, true},
		"no operator":                   {api, "", analyzer, true, true},
		"no analyzer configured":        {api, operator, "", false, true},
		"short API token":               {"short", operator, analyzer, true, false},
		"operator reuses the API token": {api, api, analyzer, true, false},
		"short operator token":          {api, "short", analyzer, true, false},
		"analyzer reuses the API token": {api, operator, api, true, false},
		"analyzer reuses the operator":  {api, operator, operator, true, false},
		"analyzer token missing":        {api, operator, "", true, false},
	} {
		if err := checkTokens(tc.api, tc.operator, tc.analyzer, tc.configured); (err == nil) != tc.ok {
			t.Errorf("%s: checkTokens = %v, want ok=%v", name, err, tc.ok)
		}
	}
}
