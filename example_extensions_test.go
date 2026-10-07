package nmea_test

import (
	"fmt"

	"github.com/adrianmo/go-nmea"
)

// ExampleParse_extensions shows that sentences parsed through the existing
// Parse interface keep their typed, known fields and additionally expose unknown
// trailing ("reserved") extension fields, including an empty tail field.
func ExampleParse_extensions() {
	sentences := []string{
		"$FBALC,02,01,03,01,FEB,01,02,03*0A",       // standard sentence, no tail
		"$FBALC,02,01,03,01,FEB,01,02,03,XX,YY*0A", // two extension fields
		"$FBALC,02,01,03,01,FEB,01,02,03,XX,*0A",   // empty trailing extension field
		"$FBALC,02,01*49",                          // mandatory field missing
	}

	for _, raw := range sentences {
		m, err := nmea.Parse(raw)
		if err != nil {
			fmt.Printf("error: %v\n", err)
			continue
		}

		alc := m.(nmea.ALC)
		fmt.Printf("entries=%d firstAlertID=%d",
			alc.EntriesNumber, alc.AlertEntries[0].AlertIdentifier)

		if ext, ok := m.(nmea.ExtensibleSentence); ok {
			for _, f := range ext.Extensions() {
				fmt.Printf(" [%d]=%q", f.Index, f.Value)
			}
		}
		fmt.Println()
	}

	// Output:
	// entries=1 firstAlertID=1
	// entries=1 firstAlertID=1 [8]="XX" [9]="YY"
	// entries=1 firstAlertID=1 [8]="XX" [9]=""
	// error: nmea: FBALC invalid message ID: index out of range
}
