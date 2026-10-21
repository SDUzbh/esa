package esa

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// allocated reports the bytes ParseImport allocates for one upload.
func allocated(t *testing.T, filename, content string) (Preview, error, uint64) {
	t.Helper()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	p, err := ParseImport(filename, "1", []byte(content), time.Unix(0, 0))
	runtime.ReadMemStats(&after)
	return p, err, after.TotalAlloc - before.TotalAlloc
}

// Crafted uploads near the 12 MB body limit used to cost 750–1,000 MB of
// allocation; each is now rejected while costing a small multiple of its size.
func TestImportRejectsOversizedUploadsCheaply(t *testing.T) {
	const size = 12 << 20
	for name, c := range map[string]struct{ file, content, refusal string }{
		"array of empty objects": {"x.json", "[" + strings.Repeat("{},", size/3-1) + "{}]", "at most 2000 tickets"},
		"array of small numbers": {"x.json", "[" + strings.Repeat("1,", size/2-1) + "1]", "at most 2000 tickets"},
		"issues array":           {"x.json", `{"issues":[` + strings.Repeat("{},", size/3-1) + "{}]}", "at most 2000 tickets"},
		"very wide CSV header":   {"x.csv", strings.Repeat("a,", size/2-1) + "a\n", "more than 1024 columns"},
		"very wide CSV row":      {"x.csv", "subject\n" + strings.Repeat("a,", size/2-1) + "a\n", "more than 1024 columns"},
	} {
		_, err, bytes := allocated(t, c.file, c.content)
		if err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%s: %v", name, err)
		}
		if bytes > 4*uint64(len(c.content)) {
			t.Errorf("%s: allocated %d MB for a %d MB upload", name, bytes>>20, len(c.content)>>20)
		}
	}
}

func TestImportBoundsEachRecord(t *testing.T) {
	tiny := `{"subject":"many values","x":[` + strings.Repeat("1,", maxImportRecordValues) + "1]}"
	huge := `{"subject":"huge","description":"` + strings.Repeat("x", maxImportRecordBytes) + `"}`
	content := "[" + strings.Join([]string{`{"subject":"kept"}`, tiny, huge, `{"subject":"also kept"}`}, ",") + "]"
	p, err, _ := allocated(t, "x.json", content)
	if err != nil || len(p.Tickets) != 2 || p.Tickets[0].Subject != "kept" || p.Tickets[1].Subject != "also kept" {
		t.Fatal("records within the bounds were not kept", p.Tickets, err)
	}
	if len(p.Problems) != 2 || p.Problems[0].Record != 2 || !strings.Contains(p.Problems[0].Message, "JSON values") ||
		p.Problems[1].Record != 3 || !strings.Contains(p.Problems[1].Message, "larger than 8 MB") {
		t.Fatal("oversized records were not reported as their own problems", p.Problems)
	}
	// A CSV field over the bound is that row's problem.
	csv := "subject,description\nkept,short\nbig," + strings.Repeat("y", maxImportCSVField+1) + "\n"
	if p, err, _ = allocated(t, "x.csv", csv); err != nil || len(p.Tickets) != 1 || len(p.Problems) != 1 || p.Problems[0].Record != 2 {
		t.Fatal("an oversized CSV field was not that row's problem", p.Problems, err)
	}
}

func TestImportCSVColumnCountIgnoresQuotedText(t *testing.T) {
	// Commas, doubled quotes and newlines inside quotes are not columns.
	quoted := `"` + strings.Repeat(`a, ""b"",`+"\n", 2000) + `"`
	content := "subject,description\nkept," + quoted + "\n"
	if got := csvMaxColumns([]byte(content)); got != 2 {
		t.Fatal("quoted text was counted as columns:", got)
	}
	p, err, _ := allocated(t, "x.csv", content)
	if err != nil || len(p.Tickets) != 1 || !strings.Contains(p.Tickets[0].Description, `"b"`) {
		t.Fatal(p.Problems, err)
	}
	wide := strings.Repeat("c,", maxImportCSVColumns-1) + "subject\n"
	if got := csvMaxColumns([]byte(wide)); got != maxImportCSVColumns {
		t.Fatal(got)
	}
	if _, err = ParseImport("x.csv", "1", []byte(wide+strings.Repeat(",", maxImportCSVColumns-1)+"s\n"), time.Unix(0, 0)); err != nil {
		t.Fatal("a header at the column bound was rejected", err)
	}
}

// jsonValues is an upper bound: one per object or array, separator and the
// first value; text inside strings, escaped quotes included, is not counted.
func TestJSONValueCount(t *testing.T) {
	for raw, want := range map[string]int{
		`1`:                        1,
		`{"a":1,"b":[1,2,{}]}`:     7,
		`["x,{[", "\"[,", [], {}]`: 7,
	} {
		if got := jsonValues([]byte(raw)); got != want {
			t.Errorf("%s: %d, want %d", raw, got, want)
		}
	}
}
