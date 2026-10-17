package esa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// importChunkBytes bounds the payload text sent in one multi-row insert.
const importChunkBytes = 8 << 20

// Import writes a complete state into a pristine Postgres schema in one
// transaction, reads it back inside that transaction, and commits only if the
// stored state reproduces the source byte for byte, both completely and as the
// working set a Load returns. The source is not modified. It requires
// AcquireWriter, so no other ESA process can write meanwhile.
func (p *PostgresPersister) Import(ctx context.Context, source State) error {
	if p.term == 0 {
		return errors.New("esa: Postgres Import before AcquireWriter")
	}
	if err := validateState(source); err != nil {
		return err
	}
	source = source.detach()
	compactLegacyEvents(&source)
	tx, err := p.conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tables := []string{"case_history", "membership_history", "event_raw"}
	for _, table := range pgTables {
		tables = append(tables, table.name)
	}
	var revision int64
	if err = tx.QueryRow(ctx, "SELECT revision FROM "+p.table("meta")).Scan(&revision); err != nil {
		return err
	}
	for _, table := range tables {
		var used bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM "+p.table(table)+")").Scan(&used); err != nil {
			return err
		}
		if used || revision != 0 {
			return fmt.Errorf("%w: state import requires a pristine database (revision %d, %s has rows: %v)", ErrConflict, revision, table, used)
		}
	}
	insert := func(table string, columns string, values string, rows [][]string) error {
		for start := 0; start < len(rows); {
			end, size := start, 0
			for end < len(rows) && (end == start || size < importChunkBytes) {
				for _, v := range rows[end] {
					size += len(v)
				}
				end++
			}
			args := make([]any, len(rows[0]))
			for i := range args {
				column := make([]string, 0, end-start)
				for _, row := range rows[start:end] {
					column = append(column, row[i])
				}
				args[i] = column
			}
			if _, err := tx.Exec(ctx, "INSERT INTO "+p.table(table)+" ("+columns+") SELECT "+values+" FROM "+unnestRows(len(args)), args...); err != nil {
				return fmt.Errorf("import %s: %w", table, err)
			}
			start = end
		}
		return nil
	}
	for _, table := range pgTables {
		ids := table.ids(&source)
		slices.Sort(ids)
		rows := make([][]string, 0, len(ids))
		for _, id := range ids {
			payload, err := table.encode(&source, id)
			if err != nil {
				return fmt.Errorf("%s %s: %w", table.name, id, err)
			}
			rows = append(rows, []string{id, string(payload)})
		}
		if len(rows) > 0 {
			if err = insert(table.name, "id, payload, revision", "r.a, r.b::json, 1", rows); err != nil {
				return err
			}
		}
	}
	var raws [][]string
	for id, r := range source.Events.All() {
		if len(r.Raw) > 0 {
			raws = append(raws, []string{id, string(r.Raw)})
		}
	}
	if len(raws) > 0 {
		if err = insert("event_raw", "event_id, raw", "r.a, r.b::json", raws); err != nil {
			return err
		}
	}
	if err = importHistory(insert, "case_history", "case_id", source.CaseHistory, func(c Case) string { return c.ID }); err != nil {
		return err
	}
	if err = importHistory(insert, "membership_history", "link_id", source.MembershipHistory, func(l IssueTicketLink) string { return l.ID }); err != nil {
		return err
	}
	frame, err := json.Marshal(pgFrame{IssueAccessSequence: source.IssueAccessSequence, ReplayClock: source.ReplayClock, LastClaimedProject: source.LastClaimedProject, SchemaVersion: source.SchemaVersion, Unknown: source.StoredFields})
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "UPDATE "+p.table("meta")+" SET revision = 1, schema_version = $1, frame = $2::json WHERE revision = 0 AND writer_term = $3", source.SchemaVersion, string(frame), int64(p.term))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return p.fenced(ctx)
	}
	if p.testBeforeImportVerify != nil {
		if err = p.testBeforeImportVerify(ctx, tx); err != nil {
			return err
		}
	}
	stored, storedRevision, _, err := p.readState(ctx, tx, true)
	if err != nil {
		return fmt.Errorf("import verification: %w", err)
	}
	if err = sameStateEncoding("complete", source, stored); err != nil {
		return err
	}
	working, _, _, err := p.readState(ctx, tx, false)
	if err != nil {
		return fmt.Errorf("import verification: %w", err)
	}
	if err = sameStateEncoding("working set", hotProjection(source), hotProjection(working)); err != nil {
		return err
	}
	if storedRevision != 1 {
		return fmt.Errorf("import verification: stored revision %d, want 1", storedRevision)
	}
	return tx.Commit(ctx)
}

func importHistory[V any](insert func(string, string, string, [][]string) error, table, keyColumn string, history []V, key func(V) string) error {
	if len(history) == 0 {
		return nil
	}
	rows := make([][]string, 0, len(history))
	for i, v := range history {
		payload, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("%s %d: %w", table, i, err)
		}
		rows = append(rows, []string{fmt.Sprint(i), key(v), string(payload)})
	}
	return insert(table, "seq, "+keyColumn+", payload", "r.a::bigint, r.b, r.c::json", rows)
}

// unnestRows returns "unnest($1::text[], ...) AS r(a, ...)" for n columns.
func unnestRows(n int) string {
	args, columns := make([]string, n), make([]string, n)
	for i := range n {
		args[i], columns[i] = fmt.Sprintf("$%d::text[]", i+1), string(rune('a'+i))
	}
	return "unnest(" + strings.Join(args, ", ") + ") AS r(" + strings.Join(columns, ", ") + ")"
}

// hotProjection is the part of s that a working-set Load reproduces: snapshots
// referenced in memory, no membership history and no raw event payloads.
func hotProjection(s State) State {
	out := s.detach()
	hot := map[string]bool{}
	for _, id := range hotSnapshotIDs(&s) {
		hot[id] = true
	}
	for id := range out.Snapshots.All() {
		if !hot[id] {
			out.Snapshots.Delete(id)
		}
	}
	for id, r := range out.Events.All() {
		r.Raw = nil
		out.Events.Put(id, r)
	}
	out.MembershipHistory = nil
	return out
}

// sameStateEncoding compares two states by their JSON encoding, treating
// empty and absent collections alike, and names the first field that differs.
func sameStateEncoding(what string, want, got State) error {
	normalize := func(s State) map[string]json.RawMessage {
		s = s.detach()
		if len(s.CaseHistory) == 0 {
			s.CaseHistory = nil
		}
		if len(s.MembershipHistory) == 0 {
			s.MembershipHistory = nil
		}
		b, _ := json.Marshal(s)
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(b, &fields)
		return fields
	}
	a, b := normalize(want), normalize(got)
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if string(a[k]) != string(b[k]) {
			return fmt.Errorf("import verification failed: stored %s %q differs from the source", what, k)
		}
	}
	return nil
}
