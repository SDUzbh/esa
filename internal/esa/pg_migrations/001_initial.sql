-- ESA state schema, version 1. {{schema}} is replaced by the configured schema name.
-- Payloads use the json type, not jsonb, so stored text round-trips exactly.

CREATE TABLE {{schema}}.meta (
  singleton      boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  schema_version int     NOT NULL,
  revision       bigint  NOT NULL,  -- +1 per committed Update
  writer_term    bigint  NOT NULL,  -- +1 each time a process becomes the writer
  frame          json    NOT NULL   -- scalar State fields and unknown top-level fields
);
INSERT INTO {{schema}}.meta (schema_version, revision, writer_term, frame) VALUES (1, 0, 0, '{"schema_version":1}');

-- One table per State map. revision is the meta.revision that last wrote the row.
CREATE TABLE {{schema}}.latest_snapshot (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.snapshots (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.engineering_issues (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.issue_ticket_links (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.issue_overrides (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.investigations (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.investigation_groups (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.solution_revisions (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.classifier_predictions (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.classifier_feedback (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.tickets (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.events (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.receipts (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.event_rejections (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.runs (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.latest (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);
CREATE TABLE {{schema}}.cases (id text PRIMARY KEY, project_id text GENERATED ALWAYS AS (payload->>'project_id') STORED, payload json NOT NULL, revision bigint NOT NULL);

-- Append-only histories; seq is the position in the State slice.
CREATE TABLE {{schema}}.case_history (seq bigint PRIMARY KEY, case_id text NOT NULL, payload json NOT NULL);
CREATE TABLE {{schema}}.membership_history (seq bigint PRIMARY KEY, link_id text NOT NULL, payload json NOT NULL);
-- Cold: never loaded into memory.
CREATE TABLE {{schema}}.event_raw (event_id text PRIMARY KEY, raw json NOT NULL);

-- Snapshots, solution revisions and histories are immutable once written.
CREATE FUNCTION {{schema}}.reject_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'esa: % rows are immutable', TG_TABLE_NAME;
END
$$;
CREATE TRIGGER snapshots_immutable BEFORE UPDATE OR DELETE ON {{schema}}.snapshots FOR EACH ROW EXECUTE FUNCTION {{schema}}.reject_change();
CREATE TRIGGER solution_revisions_immutable BEFORE UPDATE OR DELETE ON {{schema}}.solution_revisions FOR EACH ROW EXECUTE FUNCTION {{schema}}.reject_change();
CREATE TRIGGER case_history_immutable BEFORE UPDATE OR DELETE ON {{schema}}.case_history FOR EACH ROW EXECUTE FUNCTION {{schema}}.reject_change();
CREATE TRIGGER membership_history_immutable BEFORE UPDATE OR DELETE ON {{schema}}.membership_history FOR EACH ROW EXECUTE FUNCTION {{schema}}.reject_change();
CREATE TRIGGER event_raw_immutable BEFORE UPDATE OR DELETE ON {{schema}}.event_raw FOR EACH ROW EXECUTE FUNCTION {{schema}}.reject_change();
