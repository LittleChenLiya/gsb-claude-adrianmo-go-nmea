package nmea

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestALC(t *testing.T) {
	var tests = []struct {
		name string
		raw  string
		err  string
		msg  ALC
	}{
		{
			name: "good sentence, single entry",
			raw:  "$FBALC,02,01,03,01,FEB,01,02,03*0A",
			msg: ALC{
				NumFragments:   2,
				FragmentNumber: 1,
				MessageID:      3,
				EntriesNumber:  1,
				AlertEntries: []ALCAlertEntry{
					{
						ManufacturerMnemonicCode: "FEB",
						AlertIdentifier:          01,
						AlertInstance:            02,
						RevisionCounter:          03,
					},
				},
			},
		},
		{
			name: "good sentence, multiple entries",
			raw:  "$FBALC,02,01,03,02,FEB,01,02,03,TEB,02,03,04*5f",
			msg: ALC{
				NumFragments:   2,
				FragmentNumber: 1,
				MessageID:      3,
				EntriesNumber:  2,
				AlertEntries: []ALCAlertEntry{
					{
						ManufacturerMnemonicCode: "FEB",
						AlertIdentifier:          01,
						AlertInstance:            02,
						RevisionCounter:          03,
					},
					{
						ManufacturerMnemonicCode: "TEB",
						AlertIdentifier:          02,
						AlertInstance:            03,
						RevisionCounter:          04,
					},
				},
			},
		},
		{
			name: "good sentence, no entries",
			raw:  "$FBALC,02,01,03,00*4a",
			msg: ALC{
				NumFragments:   2,
				FragmentNumber: 1,
				MessageID:      3,
				EntriesNumber:  0,
				AlertEntries:   nil,
			},
		},
		{
			name: "good sentence, two unknown extension fields",
			raw:  "$FBALC,02,01,03,01,FEB,01,02,03,XX,YY*0A",
			msg: ALC{
				NumFragments:   2,
				FragmentNumber: 1,
				MessageID:      3,
				EntriesNumber:  1,
				AlertEntries: []ALCAlertEntry{
					{
						ManufacturerMnemonicCode: "FEB",
						AlertIdentifier:          01,
						AlertInstance:            02,
						RevisionCounter:          03,
					},
				},
				ExtensionFields: []ExtensionField{
					{Index: 8, Value: "XX"},
					{Index: 9, Value: "YY"},
				},
			},
		},
		{
			name: "good sentence, empty trailing extension field is kept",
			raw:  "$FBALC,02,01,03,01,FEB,01,02,03,XX,*0A",
			msg: ALC{
				NumFragments:   2,
				FragmentNumber: 1,
				MessageID:      3,
				EntriesNumber:  1,
				AlertEntries: []ALCAlertEntry{
					{
						ManufacturerMnemonicCode: "FEB",
						AlertIdentifier:          01,
						AlertInstance:            02,
						RevisionCounter:          03,
					},
				},
				ExtensionFields: []ExtensionField{
					{Index: 8, Value: "XX"},
					{Index: 9, Value: ""},
				},
			},
		},
		{
			name: "good sentence, reserved tail on a sentence with no entries",
			raw:  "$FBALC,02,01,03,00,XX,YY*4A",
			msg: ALC{
				NumFragments:    2,
				FragmentNumber:  1,
				MessageID:       3,
				EntriesNumber:   0,
				AlertEntries:    nil,
				ExtensionFields: []ExtensionField{{Index: 4, Value: "XX"}, {Index: 5, Value: "YY"}},
			},
		},
		{
			name: "invalid nmea: fields missing for the declared alert entries",
			raw:  "$FBALC,02,01,03,01,FEB,01,02*25",
			err:  "ALC is missing fields for the declared alert entries",
		},
		{
			name: "invalid nmea: mandatory header missing",
			raw:  "$FBALC,02,01*49",
			err:  "nmea: FBALC invalid message ID: index out of range",
		},
		{
			name: "invalid nmea: no fields",
			raw:  "$FBALC*4A",
			err:  "nmea: FBALC invalid number of fragments: index out of range",
		},
		{
			name: "invalid nmea: NumFragments",
			raw:  "$FBALC,0x,01,03,01,FEB,01,02,03*40",
			err:  "nmea: FBALC invalid number of fragments: 0x",
		},
		{
			name: "invalid nmea: FragmentNumber",
			raw:  "$FBALC,02,0a,03,01,FEB,01,02,03*5a",
			err:  "nmea: FBALC invalid fragment number: 0a",
		},
		{
			name: "invalid nmea: MessageID",
			raw:  "$FBALC,02,01,0x,01,FEB,01,02,03*41",
			err:  "nmea: FBALC invalid message ID: 0x",
		},
		{
			name: "invalid nmea: EntriesNumber",
			raw:  "$FBALC,02,01,03,0x,FEB,01,02,03*43",
			err:  "nmea: FBALC invalid entries number: 0x",
		},
		{
			name: "invalid nmea: AlertIdentifier",
			raw:  "$FBALC,02,01,03,01,FEB,0x,02,03*43",
			err:  "nmea: FBALC invalid alert identifier: 0x",
		},
		{
			name: "invalid nmea: AlertInstance",
			raw:  "$FBALC,02,01,03,01,FEB,01,0x,03*40",
			err:  "nmea: FBALC invalid alert instance: 0x",
		},
		{
			name: "invalid nmea: RevisionCounter",
			raw:  "$FBALC,02,01,03,01,FEB,01,02,0x*41",
			err:  "nmea: FBALC invalid revision counter: 0x",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Parse(tt.raw)
			if tt.err != "" {
				assert.Error(t, err)
				assert.EqualError(t, err, tt.err)
			} else {
				assert.NoError(t, err)
				arc := m.(ALC)
				arc.BaseSentence = BaseSentence{}
				assert.Equal(t, tt.msg, arc)
			}
		})
	}
}

// TestALCExtensionView checks the existing Parse data flow exposes the unknown
// tail through the ExtensibleSentence view while the known fields keep their
// typed parsing.
func TestALCExtensionView(t *testing.T) {
	// ALC participates in the extension field data flow.
	var _ ExtensibleSentence = ALC{}

	cases := []struct {
		name        string
		raw         string
		wantErr     bool
		entries     int
		extensions  []ExtensionField
		identifiers []int64
	}{
		{
			name:        "normal sentence without extension fields",
			raw:         "$FBALC,02,01,03,01,FEB,01,02,03*0A",
			entries:     1,
			identifiers: []int64{1},
			extensions:  nil,
		},
		{
			name:        "two extension fields",
			raw:         "$FBALC,02,01,03,01,FEB,01,02,03,XX,YY*0A",
			entries:     1,
			identifiers: []int64{1},
			extensions:  []ExtensionField{{Index: 8, Value: "XX"}, {Index: 9, Value: "YY"}},
		},
		{
			name:        "empty trailing extension field is not filtered",
			raw:         "$FBALC,02,01,03,01,FEB,01,02,03,XX,*0A",
			entries:     1,
			identifiers: []int64{1},
			extensions:  []ExtensionField{{Index: 8, Value: "XX"}, {Index: 9, Value: ""}},
		},
		{
			name:    "mandatory field missing still fails",
			raw:     "$FBALC,02,01*49",
			wantErr: true,
		},
		{
			name:    "declared entry but missing entry fields still fails",
			raw:     "$FBALC,02,01,03,01,FEB,01,02*25",
			wantErr: true,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			m, err := Parse(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			alc, ok := m.(ALC)
			assert.True(t, ok)
			assert.Len(t, alc.AlertEntries, tt.entries)

			ext, ok := m.(ExtensibleSentence)
			assert.True(t, ok)
			assert.Equal(t, tt.extensions, ext.Extensions())

			for i, want := range tt.identifiers {
				assert.Equal(t, want, alc.AlertEntries[i].AlertIdentifier)
			}
		})
	}
}
