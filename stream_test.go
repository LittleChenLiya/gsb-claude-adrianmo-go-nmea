package nmea

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

const (
	streamRMC = "$GNRMC,220516,A,5133.82,N,00042.24,W,173.8,231.8,130694,004.2,W*6E"
	streamGGA = "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*47"
	streamGSA = "$GPGSA,A,3,22,19,18,27,14,03,,,,,,,3.1,2.0,2.4*36"
)

// streamRecorder collects Streamer callbacks for assertions.
type streamRecorder struct {
	sentences []Sentence
	skips     [][]byte
	errs      []error
	raws      []string
}

func (r *streamRecorder) options() []StreamerOption {
	return []StreamerOption{
		WithOnSentence(func(s Sentence) { r.sentences = append(r.sentences, s) }),
		WithOnSkip(func(g []byte) { r.skips = append(r.skips, g) }),
		WithOnError(func(err error, raw string) {
			r.errs = append(r.errs, err)
			r.raws = append(r.raws, raw)
		}),
	}
}

func TestStreamerByteByByteDeliveredOnce(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	raw := streamRMC + "\r\n"
	for i := 0; i < len(raw); i++ {
		st.Feed([]byte{raw[i]})
		// before the terminator arrives nothing may be delivered
		if i < len(raw)-1 {
			assert.Equal(t, 0, len(rec.sentences), "delivered early at byte %d", i)
		}
	}
	assert.Equal(t, 1, len(rec.sentences), "sentence must be delivered exactly once")
	assert.Equal(t, streamRMC, rec.sentences[0].String())
	assert.IsType(t, RMC{}, rec.sentences[0])
	assert.Equal(t, 0, len(rec.errs))
	assert.Equal(t, 0, len(rec.skips))
	assert.Equal(t, 0, st.Buffered())
}

func TestStreamerGarbagePrefixSkipReceipt(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	st.Feed([]byte("\x00\x01garbage-GARBAGE" + streamGGA + "\n"))
	assert.Equal(t, 1, len(rec.skips), "garbage prefix must produce a skip receipt")
	assert.Equal(t, "\x00\x01garbage-GARBAGE", string(rec.skips[0]))
	assert.Equal(t, 1, len(rec.sentences))
	assert.IsType(t, GGA{}, rec.sentences[0])
	assert.Equal(t, 0, len(rec.errs))
}

func TestStreamerBadChecksumThenRecover(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	bad := "$GPGGA,123519,4807.038,N,01131.000,E,1,08,0.9,545.4,M,46.9,M,,*00"
	st.Feed([]byte(bad + "\r\n" + streamRMC + "\r\n"))
	assert.Equal(t, 1, len(rec.errs))
	var csErr *ChecksumError
	assert.True(t, errors.As(rec.errs[0], &csErr), "expected ChecksumError, got %T", rec.errs[0])
	assert.Equal(t, "47", csErr.Expected)
	assert.Equal(t, "00", csErr.Actual)
	// the valid sentence after the bad one must not be lost
	assert.Equal(t, 1, len(rec.sentences))
	assert.IsType(t, RMC{}, rec.sentences[0])
}

func TestStreamerUnsupportedTalkerAttribution(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	raw := "$XXFOO,1,2,3*" + Checksum("XXFOO,1,2,3")
	st.Feed([]byte(raw + "\n" + streamGSA + "\n"))
	assert.Equal(t, 1, len(rec.errs))
	var nsErr *NotSupportedError
	assert.True(t, errors.As(rec.errs[0], &nsErr), "expected NotSupportedError, got %T", rec.errs[0])
	var csErr *ChecksumError
	assert.False(t, errors.As(rec.errs[0], &csErr), "unsupported talker must not be attributed to checksum")
	assert.Equal(t, "XXFOO", nsErr.Prefix)
	assert.Equal(t, 1, len(rec.sentences))
	assert.IsType(t, GSA{}, rec.sentences[0])
}

func TestStreamerTooLongDoesNotGrowBuffer(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(append(rec.options(), WithMaxSentenceLength(64))...)
	// stream an unterminated sentence far beyond the limit
	st.Feed([]byte("$GPXXX,"))
	chunk := strings.Repeat("1", 50)
	for i := 0; i < 20; i++ {
		st.Feed([]byte(chunk))
		assert.True(t, st.Buffered() <= 64+len(chunk), "buffer keeps growing: %d", st.Buffered())
	}
	assert.True(t, len(rec.errs) > 0, "expected TooLongError")
	var tlErr *TooLongError
	assert.True(t, errors.As(rec.errs[0], &tlErr))
	assert.Equal(t, 64, tlErr.Limit)
	// and a valid sentence afterwards is still delivered
	st.Feed([]byte(streamRMC + "\n"))
	assert.Equal(t, 1, len(rec.sentences))
}

func TestStreamerUnterminatedResync(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	// partial sentence, no terminator, then a complete sentence starts
	st.Feed([]byte("$GPGGA,123519,4807.038,N,0113"))
	st.Feed([]byte(streamRMC + "\n"))
	assert.Equal(t, 1, len(rec.errs))
	var unErr *UnterminatedError
	assert.True(t, errors.As(rec.errs[0], &unErr), "expected UnterminatedError, got %T", rec.errs[0])
	assert.Equal(t, "$GPGGA,123519,4807.038,N,0113", unErr.Raw)
	assert.Equal(t, 1, len(rec.sentences), "sentence after resync must not be lost")
	assert.IsType(t, RMC{}, rec.sentences[0])
}

func TestStreamerSplitAcrossChunks(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	raw := streamRMC + "\r\n" + streamGGA + "\r\n"
	// split at every possible position, both sentences must arrive exactly once
	for split := 0; split <= len(raw); split++ {
		rec = &streamRecorder{}
		st = NewStreamer(rec.options()...)
		st.Feed([]byte(raw[:split]))
		st.Feed([]byte(raw[split:]))
		assert.Equal(t, 2, len(rec.sentences), "split at %d", split)
		assert.Equal(t, 0, len(rec.errs), "split at %d", split)
	}
}

func TestStreamerMultipleSentencesOneChunk(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	st.Feed([]byte(streamRMC + "\r\n" + streamGGA + "\n" + streamGSA + "\r\n"))
	assert.Equal(t, 3, len(rec.sentences))
	assert.IsType(t, RMC{}, rec.sentences[0])
	assert.IsType(t, GGA{}, rec.sentences[1])
	assert.IsType(t, GSA{}, rec.sentences[2])
	assert.Equal(t, 0, len(rec.errs))
}

func TestStreamerTerminatorCRLFStrict(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(append(rec.options(), WithTerminator(TerminatorCRLF))...)
	// bare LF must not terminate under the strict CRLF policy
	st.Feed([]byte(streamRMC + "\n"))
	assert.Equal(t, 0, len(rec.sentences))
	// the next explicit start resynchronizes; CRLF-terminated sentence works
	st.Feed([]byte(streamGGA + "\r\n"))
	assert.Equal(t, 1, len(rec.sentences))
	assert.IsType(t, GGA{}, rec.sentences[0])
	assert.Equal(t, 1, len(rec.errs))
	var unErr *UnterminatedError
	assert.True(t, errors.As(rec.errs[0], &unErr))
}

func TestStreamerTerminatorAnyAcceptsLFandCRLF(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...) // TerminatorAny is the default
	st.Feed([]byte(streamRMC + "\n" + streamGGA + "\r\n"))
	assert.Equal(t, 2, len(rec.sentences))
	assert.Equal(t, streamRMC, rec.sentences[0].String())
	assert.Equal(t, streamGGA, rec.sentences[1].String())
	assert.Equal(t, 0, len(rec.errs))
}

func TestStreamerTagBlockSentence(t *testing.T) {
	rec := &streamRecorder{}
	st := NewStreamer(rec.options()...)
	raw := `\s:Satelite_1,c:1553390539*62\!AIVDM,2,2,4,B,00000000000,2*23`
	// verify the line is accepted by the classic parser first
	if _, err := Parse(raw); err != nil {
		t.Skipf("fixture not supported by parser: %v", err)
	}
	st.Feed([]byte(raw + "\r\n"))
	assert.Equal(t, 1, len(rec.sentences))
	assert.Equal(t, 0, len(rec.errs))
}

func ExampleStreamer() {
	st := NewStreamer(
		WithOnSentence(func(s Sentence) { fmt.Println(s.Prefix()) }),
		WithOnSkip(func(g []byte) { fmt.Printf("skipped %q\n", g) }),
		WithOnError(func(err error, raw string) { fmt.Println("error:", err) }),
	)
	st.Feed([]byte("noise" + streamRMC + "\r\n"))
	// Output:
	// skipped "noise"
	// GNRMC
}
