package nmea

import (
	"errors"
	"fmt"
)

// ExampleStreamReader feeds an arbitrary, chunked byte stream containing a
// garbage prefix and three NMEA0183 sentences to an incremental StreamReader.
// Every sentence is delivered exactly once no matter where the chunk
// boundaries fall, the garbage prefix gets an explicit skip receipt, and the
// valid sentence after the bad-checksum one is recovered.
//
// Run with:
//
//	go test -run ExampleStreamReader -v
func ExampleStreamReader() {
	// Three well-formed sentences: a $ sentence, another $ sentence and an
	// encapsulated ! sentence. The first copy of the RMC sentence is corrupted
	// by flipping one checksum digit so resynchronisation can be demonstrated.
	rmc := "$GPRMC,220516,A,5133.82,N,00042.24,W,173.8,231.8,130694,004.2,W*70"
	gga := "$GPGGA,034225.077,3356.4650,S,15124.5567,E,1,03,9.7,-25.0,M,21.0,M,,0000*51"
	vdm := "!AIVDM,1,1,,A,13aGt0PP0jPN@9fMPKVDJgwfR>`<,0*55"
	badRMC := rmc[:len(rmc)-1] + "1" // ...*71 -> checksum mismatch

	stream := "garbage-prefix\r\n" + badRMC + "\r\n" + gga + "\r\n" + vdm

	reader := NewStreamReader(func(e Event) {
		switch e.Kind {
		case EventSentence:
			fmt.Printf("%s: %s\n", e.Kind, e.Sentence.DataType())
		case EventChecksumError:
			var ce *ChecksumError
			if errors.As(e.Err, &ce) {
				fmt.Printf("%s: %s (expected %s)\n", e.Kind, ce.Got, ce.Expected)
			}
		case EventUnsupportedSentence:
			fmt.Printf("%s\n", e.Kind)
		case EventMalformed, EventTooLong, EventSkipped:
			fmt.Printf("%s: %q\n", e.Kind, string(e.Data))
		}
	})

	// Write the stream in two arbitrary chunks to show boundaries do not
	// matter; one byte at a time would work just as well.
	split := len(stream) / 2
	if _, err := reader.Write([]byte(stream[:split])); err != nil {
		panic(err)
	}
	if _, err := reader.Write([]byte(stream[split:])); err != nil {
		panic(err)
	}
	reader.Flush()

	// Output:
	// skipped bytes: "garbage-prefix\r\n"
	// checksum error: 71 (expected 70)
	// skipped bytes: "\r\n"
	// sentence: GGA
	// sentence: VDM
}
