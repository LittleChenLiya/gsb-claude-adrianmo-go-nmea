package nmea

import "errors"

const (
	// TypeALC type of ALC sentence for cyclic alert list
	TypeALC = "ALC"
)

// ALC - Cyclic alert list
// https://fcc.report/FCC-ID/ADB9ZWRTR100/2768717.pdf (page 6) FURUNO MARINE RADAR, model FAR-15XX manual
//
// Format: $--ALC,xx,xx,xx,xx, aaa,x.x,x.x,x.x,’’’’’’’’’,*hh<CR><LF>
// Example: $FBALC,02,01,03,01,FEB,01,02,03*0A
type ALC struct {
	BaseSentence

	// NumFragments is total number of ALC sentences this message (01, 16)
	NumFragments int64 // 0

	//  FragmentNumber is current fragment/sentence number (01 - 16)
	FragmentNumber int64 // 1

	// MessageID is sequential message identifier (00 - 99)
	MessageID int64 // 2

	// Number of alert entries (0 - 3)
	EntriesNumber int64 // 3

	// Additional alert entries. Each entry identifies a certain alert with a certain state.
	// It is not allowed that an alert entry is split between two ALC sentences
	AlertEntries []ALCAlertEntry // 4..

	// ExtensionFields holds unknown trailing ("reserved") fields appended after the
	// known alert entries. They are not parsed, empty trailing fields are kept, and
	// each value is stored together with its original field index.
	ExtensionFields []ExtensionField
}

// Extensions returns the unknown trailing fields of the ALC sentence.
// It implements the ExtensibleSentence interface.
func (a ALC) Extensions() []ExtensionField {
	return a.ExtensionFields
}

// ALCAlertEntry is instance of alert entry for ALC sentence
type ALCAlertEntry struct {
	// ManufacturerMnemonicCode is manufacturer mnemonic code
	ManufacturerMnemonicCode string // i+4

	// AlertIdentifier is alert identifier (001 to 99999)
	AlertIdentifier int64 // i+5

	// AlertInstance is alert instance
	AlertInstance int64 // i+6

	// RevisionCounter is revision counter (1 - 99)
	RevisionCounter int64 // i+7
}

// newALC constructor
func newALC(s BaseSentence, opts ...ParserOption) (Sentence, error) {
	p := NewParser(s, opts...)
	p.AssertType(TypeALC)
	alc := ALC{
		BaseSentence:   s,
		NumFragments:   p.Int64(0, "number of fragments"),
		FragmentNumber: p.Int64(1, "fragment number"),
		MessageID:      p.Int64(2, "message ID"),
		EntriesNumber:  p.Int64(3, "entries number"),
		AlertEntries:   nil,
	}
	if p.Err() != nil {
		return alc, p.Err()
	}

	// Known fields are bounded by the declared number of entries: the four mandatory
	// header fields are followed by EntriesNumber data sets of four fields each.
	if alc.EntriesNumber < 0 {
		return alc, errors.New("ALC entries number must not be negative")
	}
	knownFieldCount := 4 + int(alc.EntriesNumber)*4
	if knownFieldCount > len(p.Fields) {
		return alc, errors.New("ALC is missing fields for the declared alert entries")
	}

	if alc.EntriesNumber > 0 {
		alc.AlertEntries = make([]ALCAlertEntry, 0, alc.EntriesNumber)
	}
	for i := 0; i < int(alc.EntriesNumber); i++ {
		offset := 4 + i*4
		tmp := ALCAlertEntry{
			ManufacturerMnemonicCode: p.String(offset, "manufacturer mnemonic code"),
			AlertIdentifier:          p.Int64(offset+1, "alert identifier"),
			AlertInstance:            p.Int64(offset+2, "alert instance"),
			RevisionCounter:          p.Int64(offset+3, "revision counter"),
		}
		alc.AlertEntries = append(alc.AlertEntries, tmp)
	}
	if p.Err() != nil {
		return alc, p.Err()
	}

	// Everything after the known fields is an unknown tail: keep the raw value and
	// the original position of each field, including empty trailing fields.
	alc.ExtensionFields = p.Extensions(knownFieldCount)

	return alc, p.Err()
}
