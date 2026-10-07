package nmea

// ExtensionField is a trailing NMEA0183 field that is not described by the sentence
// definition. Some devices append proprietary or future "reserved" fields to
// otherwise variable-length sentences.
//
// Such fields are not parsed into typed sentence fields. They are kept verbatim
// (including empty values) together with their original field index so callers
// can still access the raw bytes and the position they had in the sentence.
type ExtensionField struct {
	// Index is the zero based position of the field within the sentence data
	// fields (the address field, e.g. "FBALC", is field -1 and is not included).
	Index int

	// Value is the raw, unparsed field value. Empty strings are preserved and
	// are not filtered out.
	Value string
}

// ExtensibleSentence is implemented by sentences that preserve unknown trailing
// extension fields. The known, documented fields keep being parsed into the typed
// sentence fields, while every field past the end of the known definition is
// returned here with its original value and position.
type ExtensibleSentence interface {
	Sentence

	// Extensions returns the unknown trailing fields of the sentence, in order.
	// Empty trailing fields are retained. It returns nil when the sentence has
	// no extension fields.
	Extensions() []ExtensionField
}
