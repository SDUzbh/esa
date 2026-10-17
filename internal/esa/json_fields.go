package esa

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
)

// Unknown persisted fields survive a read/write cycle as inert data. No
// workflow interprets retired protocol fields or runs their former behavior.
type JSONFields map[string]json.RawMessage

// decodeFields decodes data into target, keeps unknown fields in extra, and
// fails if a required field is absent or null.
func decodeFields(data []byte, target any, extra *JSONFields, required ...string) error {
	*extra = nil
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	var fields JSONFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, name := range required {
		if raw, ok := fields[name]; !ok || string(raw) == "null" {
			return errors.New("invalid store schema")
		}
	}
	typ := reflect.TypeOf(target).Elem()
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			delete(fields, name)
		}
	}
	if len(fields) > 0 {
		*extra = fields
	}
	return nil
}
func encodeFields(value any, extra JSONFields) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil || len(extra) == 0 {
		return raw, err
	}
	var fields JSONFields
	if err = json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for name, data := range extra {
		if _, known := fields[name]; !known {
			fields[name] = data
		}
	}
	return json.Marshal(fields)
}
func (t *Ticket) UnmarshalJSON(data []byte) error {
	type plain Ticket
	return decodeFields(data, (*plain)(t), &t.StoredFields)
}
func (t Ticket) MarshalJSON() ([]byte, error) {
	type plain Ticket
	return encodeFields(plain(t), t.StoredFields)
}
func (t *Case) UnmarshalJSON(data []byte) error {
	type plain Case
	return decodeFields(data, (*plain)(t), &t.StoredFields)
}
func (t Case) MarshalJSON() ([]byte, error) {
	type plain Case
	return encodeFields(plain(t), t.StoredFields)
}
func (t *Report) UnmarshalJSON(data []byte) error {
	type plain Report
	return decodeFields(data, (*plain)(t), &t.StoredFields)
}
func (t Report) MarshalJSON() ([]byte, error) {
	type plain Report
	return encodeFields(plain(t), t.StoredFields)
}
func (t *Run) UnmarshalJSON(data []byte) error {
	type plain Run
	return decodeFields(data, (*plain)(t), &t.StoredFields)
}
func (t Run) MarshalJSON() ([]byte, error) {
	type plain Run
	return encodeFields(plain(t), t.StoredFields)
}

// stateCoreTables must be present in a stored JSON frame.
var stateCoreTables = []string{"tickets", "events", "receipts", "runs", "latest", "cases"}

func (t *State) UnmarshalJSON(data []byte) error {
	type plain State
	return decodeFields(data, (*plain)(t), &t.StoredFields, stateCoreTables...)
}
func (t State) MarshalJSON() ([]byte, error) {
	type plain State
	return encodeFields(plain(t), t.StoredFields)
}
func (t *SolutionRevision) UnmarshalJSON(data []byte) error {
	type plain SolutionRevision
	return decodeFields(data, (*plain)(t), &t.StoredFields)
}
func (t SolutionRevision) MarshalJSON() ([]byte, error) {
	type plain SolutionRevision
	return encodeFields(plain(t), t.StoredFields)
}
func (t *TaskContract) UnmarshalJSON(data []byte) error {
	type plain TaskContract
	return decodeFields(data, (*plain)(t), &t.StoredFields)
}
func (t TaskContract) MarshalJSON() ([]byte, error) {
	type plain TaskContract
	return encodeFields(plain(t), t.StoredFields)
}
