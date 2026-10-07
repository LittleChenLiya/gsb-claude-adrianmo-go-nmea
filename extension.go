package nmea

// ExtensionField is a trailing data field of a sentence that lies beyond the
// fields known to (parsed by) this library. The raw string value and the
// position of the field within BaseSentence.Fields are preserved so callers
// can inspect vendor- or revision-specific tail data without the library
// having to understand it.
type ExtensionField struct {
	Index int    // position of the field within BaseSentence.Fields
	Value string // raw field value; empty fields are preserved as ""
}

// ExtensionFields returns a view of the raw fields from index `from` up to the
// end of the sentence. Every field keeps its original string value (empty
// fields are preserved, never filtered out) and its position within
// BaseSentence.Fields. The view is read-only: it does not alter the parser's
// error state, so a missing required field still fails parsing as before.
// If there are no fields at or after `from`, an empty slice is returned.
func (p *Parser) ExtensionFields(from int) []ExtensionField {
	ext := []ExtensionField{}
	for i := from; i < len(p.Fields); i++ {
		ext = append(ext, ExtensionField{Index: i, Value: p.Fields[i]})
	}
	return ext
}
