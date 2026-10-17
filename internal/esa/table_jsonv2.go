//go:build goexperiment.jsonv2

package esa

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
)

// MarshalJSONTo streams the table into the encoder, so encoding/json does not
// re-parse MarshalJSON's output, which dominated whole-state encoding. Values
// inherit the encoder's options, so the bytes match MarshalJSON's.
func (t Table[V]) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginObject); err != nil {
		return err
	}
	for _, e := range t.sortedEntries() {
		if err := enc.WriteToken(jsontext.String(e.key)); err != nil {
			return err
		}
		if err := jsonv2.MarshalEncode(enc, e.val); err != nil {
			return err
		}
	}
	return enc.WriteToken(jsontext.EndObject)
}
