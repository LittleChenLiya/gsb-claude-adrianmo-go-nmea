package nmea

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParserExtensionFields(t *testing.T) {
	p := NewParser(BaseSentence{
		Talker: "GN",
		Type:   "RMC",
		Fields: []string{"a", "b", "", "d"},
	})

	// view from index 1 keeps raw values (including the empty field) and positions
	assert.Equal(t, []ExtensionField{
		{Index: 1, Value: "b"},
		{Index: 2, Value: ""},
		{Index: 3, Value: "d"},
	}, p.ExtensionFields(1))

	// no fields at or after `from` yields an empty view
	assert.Equal(t, []ExtensionField{}, p.ExtensionFields(4))
	assert.Equal(t, []ExtensionField{}, p.ExtensionFields(99))

	// the view is read-only and does not set a parser error
	assert.NoError(t, p.Err())
}
