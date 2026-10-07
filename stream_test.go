package nmea

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	// Sentences taken from the package test corpus, all checksum valid.
	streamRMC = "$GPRMC,220516,A,5133.82,N,00042.24,W,173.8,231.8,130694,004.2,W*70"
	streamGGA = "$GPGGA,034225.077,3356.4650,S,15124.5567,E,1,03,9.7,-25.0,M,21.0,M,,0000*51"
	streamVDM = "!AIVDM,1,1,,A,13aGt0PP0jPN@9fMPKVDJgwfR>`<,0*55"
	// Valid RMC sentence whose checksum contains a hex letter (3F).
	streamRMCLetterCS = "$GPRMC,142754.0,A,4302.539570,N,07920.379823,W,0.0,,070617,0.0,E,A*3F"
)

// badChecksumSentence returns raw with its second checksum hex digit flipped so
// that the checksum is guaranteed to be wrong.
func badChecksumSentence(raw string) string {
	b := raw[len(raw)-1]
	switch b {
	case '0':
		b = '1'
	default:
		b = '0'
	}
	return raw[:len(raw)-1] + string(b)
}

type streamRecorder struct {
	events []Event
}

func (s *streamRecorder) handler(e Event) {
	// Copy Data so later buffer reuse inside the reader cannot affect records.
	if e.Data != nil {
		d := make([]byte, len(e.Data))
		copy(d, e.Data)
		e.Data = d
	}
	s.events = append(s.events, e)
}

func kinds(events []Event) []EventKind {
	k := make([]EventKind, len(events))
	for i, e := range events {
		k[i] = e.Kind
	}
	return k
}

func feedByteWise(t *testing.T, r *StreamReader, data string) {
	t.Helper()
	for i := 0; i < len(data); i++ {
		n, err := r.Write([]byte{data[i]})
		assert.Equal(t, 1, n)
		assert.NoError(t, err)
	}
}

// TestStreamReaderByteWiseDeliversOnce feeds each sentence one byte at a time
// and checks that every sentence is delivered exactly once.
func TestStreamReaderByteWiseDeliversOnce(t *testing.T) {
	data := "garbage\r\n" + streamRMC + "\r\n" + streamGGA + "\r\n" + streamVDM + "\r\n"
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	feedByteWise(t, r, data)
	r.Flush()

	assert.Equal(t,
		[]EventKind{EventSkipped, EventSentence, EventSentence, EventSentence},
		kinds(rec.events))
	assert.Equal(t, streamRMC, string(rec.events[1].Data))
	assert.Equal(t, streamGGA, string(rec.events[2].Data))
	assert.Equal(t, streamVDM, string(rec.events[3].Data))
	assert.Equal(t, TypeRMC, rec.events[1].Sentence.DataType())
	assert.Equal(t, TypeGGA, rec.events[2].Sentence.DataType())
	assert.Equal(t, TypeVDM, rec.events[3].Sentence.DataType())
	assert.Equal(t, "garbage\r\n", string(rec.events[0].Data))
	assert.Equal(t, 0, r.BufferedLen())
}

// TestStreamReaderArbitraryChunks replays the same stream with many different
// chunk boundaries and expects identical outcomes for every split.
func TestStreamReaderArbitraryChunks(t *testing.T) {
	data := "xx" + streamRMC + "\r\n" + streamGGA + "\r\n" + streamVDM
	for cut := 1; cut < len(data); cut++ {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler)
		for i := 0; i < len(data); i += cut {
			end := i + cut
			if end > len(data) {
				end = len(data)
			}
			_, _ = r.Write([]byte(data[i:end]))
		}
		r.Flush()
		if !assert.Equal(t,
			[]EventKind{EventSkipped, EventSentence, EventSentence, EventSentence},
			kinds(rec.events), "split size %d", cut) {
			t.FailNow()
		}
		assert.Equal(t, streamRMC, string(rec.events[1].Data))
		assert.Equal(t, streamGGA, string(rec.events[2].Data))
		assert.Equal(t, streamVDM, string(rec.events[3].Data))
		assert.Equal(t, "xx", string(rec.events[0].Data))
		assert.Equal(t, 0, r.BufferedLen())
	}
}

// TestStreamReaderBadChecksumRecovery verifies that a bad checksum is reported
// as its own error, is never handed to the parser, and that the very next
// well-formed sentence is not lost.
func TestStreamReaderBadChecksumRecovery(t *testing.T) {
	bad := badChecksumSentence(streamRMC)
	data := bad + "\r\n" + streamGGA
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	feedByteWise(t, r, data)
	r.Flush()

	assert.Equal(t,
		[]EventKind{EventChecksumError, EventSkipped, EventSentence},
		kinds(rec.events))
	assert.Nil(t, rec.events[0].Sentence)
	var ce *ChecksumError
	assert.True(t, errors.As(rec.events[0].Err, &ce))
	assert.Equal(t, bad, string(rec.events[0].Data))
	assert.Equal(t, "71", ce.Got)
	assert.Equal(t, "70", ce.Expected)
	assert.Equal(t, "\r\n", string(rec.events[1].Data))
	assert.Equal(t, streamGGA, rec.events[2].Sentence.String())
}

// TestStreamReaderUnsupportedTalker attributes an unsupported prefix
// distinctly from a bad checksum and keeps parsing the next valid sentence.
func TestStreamReaderUnsupportedTalker(t *testing.T) {
	raw := "$GPXYZ,1,2*" + Checksum("GPXYZ,1,2")
	data := raw + "\r\n" + streamRMC
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	feedByteWise(t, r, data)
	r.Flush()

	assert.Equal(t,
		[]EventKind{EventUnsupportedSentence, EventSkipped, EventSentence},
		kinds(rec.events))
	var ns *NotSupportedError
	assert.True(t, errors.As(rec.events[0].Err, &ns))
	assert.Equal(t, "GPXYZ", ns.Prefix)
	assert.Equal(t, raw, string(rec.events[0].Data))
	assert.Equal(t, streamRMC, rec.events[2].Sentence.String())
}

// TestStreamReaderNoHalfSentenceToParser ensures a half sentence is never
// handed to the parser: it is reported as malformed at Flush, and an explicit
// new start before any checksum resynchronises cleanly.
func TestStreamReaderNoHalfSentenceToParser(t *testing.T) {
	t.Run("half sentence at flush", func(t *testing.T) {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler)
		_, _ = r.Write([]byte("$GPRMC,220516,A"))
		assert.Empty(t, rec.events, "nothing emitted while incomplete")
		r.Flush()
		assert.Equal(t, []EventKind{EventMalformed}, kinds(rec.events))
		var me *MalformedError
		assert.True(t, errors.As(rec.events[0].Err, &me))
		assert.Equal(t, "$GPRMC,220516,A", me.Raw)
		assert.Equal(t, 0, r.BufferedLen())
	})

	t.Run("explicit new start resynchronises", func(t *testing.T) {
		// A '$' appears in the middle of a broken candidate. The half before it
		// is malformed; only the new candidate starting at that '$' can frame.
		// An intervening '\r' is skipped garbage between them.
		data := "$GPRMC,half\r" + streamGGA
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler)
		feedByteWise(t, r, data)
		r.Flush()
		assert.Equal(t,
			[]EventKind{EventMalformed, EventSkipped, EventSentence},
			kinds(rec.events))
		assert.Equal(t, "$GPRMC,half", string(rec.events[0].Data))
		assert.Equal(t, "\r", string(rec.events[1].Data))
		assert.Equal(t, streamGGA, rec.events[2].Sentence.String())
	})
}

// TestStreamReaderOverlongSentence checks the per-sentence length limit and
// that a valid following sentence is recovered without unbounded buffer use.
func TestStreamReaderOverlongSentence(t *testing.T) {
	const maxLen = 70
	long := bytes.Repeat([]byte{'A'}, maxLen+20)
	fake := "$" + string(long)
	data := fake + "\r\n" + streamRMC
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler, WithMaxSentenceLength(maxLen))
	feedByteWise(t, r, data)
	r.Flush()

	sawTooLong := false
	var sentences int
	for _, e := range rec.events {
		switch e.Kind {
		case EventTooLong:
			sawTooLong = true
			var te *TooLongError
			assert.True(t, errors.As(e.Err, &te))
			assert.Equal(t, maxLen, te.MaxLength)
			assert.Len(t, e.Data, maxLen)
		case EventSentence:
			sentences++
			assert.Equal(t, streamRMC, e.Sentence.String())
		}
	}
	assert.True(t, sawTooLong)
	assert.Equal(t, 1, sentences, "the valid sentence still delivered")
	assert.Equal(t, 0, r.BufferedLen())
}

// TestStreamReaderOverlongBufferBoundedByteWise verifies that feeding an
// endless stream of body bytes never grows the reader's buffer past the limit.
func TestStreamReaderOverlongBufferBoundedByteWise(t *testing.T) {
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler, WithMaxSentenceLength(32))
	for i := 0; i < 10000; i++ {
		_, _ = r.Write([]byte{'A'})
		assert.LessOrEqual(t, r.BufferedLen(), 32)
	}
	r.Flush()
	for _, e := range rec.events {
		assert.NotEqual(t, EventSentence, e.Kind)
	}
}

// TestStreamReaderGarbagePrefixReceipt checks garbage before a sentence (even
// when written across several Writes) gets a single explicit skip receipt and
// never reaches the parser.
func TestStreamReaderGarbagePrefixReceipt(t *testing.T) {
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	_, _ = r.Write([]byte("noise-before"))
	_, _ = r.Write([]byte("\r\n"))
	assert.Empty(t, rec.events, "dangling garbage stays buffered until closed")
	_, _ = r.Write([]byte(streamRMC))
	r.Flush()
	assert.Equal(t,
		[]EventKind{EventSkipped, EventSentence},
		kinds(rec.events))
	assert.Equal(t, "noise-before\r\n", string(rec.events[0].Data))
}

// TestStreamReaderTrailingGarbageAtFlush reports garbage still dangling at the
// end of the stream via Flush.
func TestStreamReaderTrailingGarbageAtFlush(t *testing.T) {
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	_, _ = r.Write([]byte(streamRMC + "trailing-noise"))
	r.Flush()
	assert.Equal(t,
		[]EventKind{EventSentence, EventSkipped},
		kinds(rec.events))
	assert.Equal(t, "trailing-noise", string(rec.events[1].Data))
	assert.Equal(t, 0, r.BufferedLen())
}

// TestStreamReaderLenientTerminator verifies the default terminator policy:
// terminator is optional, CR/LF/CRLF accepted, and other bytes become garbage
// without affecting the next sentence.
func TestStreamReaderLenientTerminator(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"none", streamRMC},
		{"lf", streamRMC + "\n"},
		{"cr", streamRMC + "\r"},
		{"crlf", streamRMC + "\r\n"},
		{"garbage then sentence", "junk" + streamRMC + "\r\njunk\r\n" + streamGGA},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &streamRecorder{}
			r := NewStreamReader(rec.handler)
			_, _ = r.Write([]byte(tc.input))
			r.Flush()
			var sentenceCount int
			for _, e := range rec.events {
				if e.Kind == EventSentence {
					sentenceCount++
				} else {
					assert.Equal(t, EventSkipped, e.Kind)
				}
			}
			if tc.name == "garbage then sentence" {
				assert.Equal(t, 2, sentenceCount)
			} else {
				assert.Equal(t, 1, sentenceCount)
			}
		})
	}
}

// TestStreamReaderStrictTerminator verifies strict CRLF handling: delivery only
// after CRLF; missing or wrong terminators are malformed; recovery still works.
func TestStreamReaderStrictTerminator(t *testing.T) {
	t.Run("delivered after CRLF", func(t *testing.T) {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler, WithStrictTerminators(true))
		_, _ = r.Write([]byte(streamRMC))
		assert.Empty(t, rec.events, "not delivered before CRLF")
		_, _ = r.Write([]byte("\r"))
		assert.Empty(t, rec.events, "not delivered before LF")
		_, _ = r.Write([]byte("\n"))
		assert.Equal(t, []EventKind{EventSentence}, kinds(rec.events))
	})

	t.Run("missing CRLF at flush is malformed", func(t *testing.T) {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler, WithStrictTerminators(true))
		_, _ = r.Write([]byte(streamRMC))
		r.Flush()
		assert.Equal(t, []EventKind{EventMalformed}, kinds(rec.events))
	})

	t.Run("wrong terminator resyncs and recovers", func(t *testing.T) {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler, WithStrictTerminators(true))
		// LF instead of CR: malformed first candidate, then the next sentence.
		_, _ = r.Write([]byte(streamRMC + "\n" + streamGGA + "\r\n"))
		assert.Equal(t,
			[]EventKind{EventMalformed, EventSkipped, EventSentence},
			kinds(rec.events))
		assert.Equal(t, streamGGA, rec.events[2].Sentence.String())
	})
}

// TestStreamReaderLineBreakBeforeChecksum aborts a candidate on CR/LF inside
// the sentence body and recovers at the next explicit start token.
func TestStreamReaderLineBreakBeforeChecksum(t *testing.T) {
	data := "$GPRMC,broken\n" + streamRMC
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	feedByteWise(t, r, data)
	r.Flush()
	assert.Equal(t,
		[]EventKind{EventMalformed, EventSkipped, EventSentence},
		kinds(rec.events))
	assert.Equal(t, "$GPRMC,broken", string(rec.events[0].Data))
	assert.Equal(t, "\n", string(rec.events[1].Data))
	assert.Equal(t, streamRMC, rec.events[2].Sentence.String())
}

// TestStreamReaderBadChecksumHex rejects checksums that are not two hex digits.
func TestStreamReaderBadChecksumHex(t *testing.T) {
	t.Run("non hex digits", func(t *testing.T) {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler)
		_, _ = r.Write([]byte("$GPXYZ,1,2*7G\r\n" + streamRMC))
		r.Flush()
		assert.Equal(t,
			[]EventKind{EventMalformed, EventSkipped, EventSentence},
			kinds(rec.events))
		assert.Equal(t, "$GPXYZ,1,2*7", string(rec.events[0].Data))
		assert.Equal(t, streamRMC, rec.events[2].Sentence.String())
	})

	t.Run("single digit at flush", func(t *testing.T) {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler)
		_, _ = r.Write([]byte("$GPXYZ,1,2*7"))
		r.Flush()
		assert.Equal(t, []EventKind{EventMalformed}, kinds(rec.events))
	})
}

// TestStreamReaderLowercaseChecksumAccepted verifies lowercase hex checksums
// are normalised before comparison (mirroring Parse behaviour).
func TestStreamReaderLowercaseChecksumAccepted(t *testing.T) {
	lower := streamRMCLetterCS[:len(streamRMCLetterCS)-2] +
		strings.ToLower(streamRMCLetterCS[len(streamRMCLetterCS)-2:])
	assert.Equal(t, "*3f", lower[len(lower)-3:])
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	_, _ = r.Write([]byte(lower + "\r\n"))
	r.Flush()
	assert.Equal(t, []EventKind{EventSentence}, kinds(rec.events))
	assert.Equal(t, TypeRMC, rec.events[0].Sentence.DataType())
}

// TestStreamReaderEventsOnceAcrossChunks is a byte-accounting test: for a stream
// with garbage, a bad checksum, a malformed half sentence and a good sentence,
// concatenating every event's data reproduces the input exactly (each byte is
// delivered exactly once) regardless of chunk boundaries, and the good
// sentence arrives exactly once.
func TestStreamReaderEventsOnceAcrossChunks(t *testing.T) {
	good2 := streamGGA
	bad := badChecksumSentence(streamRMC)
	garbage := "junk"
	half := "$GPRMC,half\r"
	data := garbage + bad + half + good2
	for _, cut := range []int{1, 2, 3, 7, 64, len(data)} {
		rec := &streamRecorder{}
		r := NewStreamReader(rec.handler)
		for i := 0; i < len(data); i += cut {
			end := i + cut
			if end > len(data) {
				end = len(data)
			}
			_, _ = r.Write([]byte(data[i:end]))
		}
		r.Flush()

		// Non-skipped events must appear in this exact order, once each.
		var significant []EventKind
		var reconstructed []byte
		var sentenceCount int
		for _, e := range rec.events {
			reconstructed = append(reconstructed, e.Data...)
			switch e.Kind {
			case EventSkipped:
			case EventSentence:
				sentenceCount++
				significant = append(significant, EventSentence)
			default:
				significant = append(significant, e.Kind)
			}
		}
		assert.Equal(t,
			[]EventKind{EventChecksumError, EventMalformed, EventSentence},
			significant, "cut %d", cut)
		assert.Equal(t, 1, sentenceCount, "cut %d", cut)
		assert.Equal(t, data, string(reconstructed), "every byte accounted once, cut %d", cut)
		assert.Equal(t, good2, rec.events[len(rec.events)-1].Sentence.String())
	}
}

// TestStreamReaderDataCopied ensures event data slices are independent copies
// and survive subsequent writes reusing the reader's internal buffer.
func TestStreamReaderDataCopied(t *testing.T) {
	rec := &streamRecorder{}
	r := NewStreamReader(rec.handler)
	_, _ = r.Write([]byte(streamRMC))
	got := rec.events[0].Data
	// Keep writing so the internal backing array would be reused/overwritten.
	feedByteWise(t, r, streamGGA+streamVDM)
	assert.Equal(t, streamRMC, string(got))
}
