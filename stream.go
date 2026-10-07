package nmea

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultMaxSentenceLength is the maximum raw NMEA0183 sentence length (in
// bytes, including the leading '$'/'!' and the two checksum hex digits, but
// excluding the trailing CR/LF) as defined by the NMEA0183 standard.
const DefaultMaxSentenceLength = 82

// EventKind describes the kind of event emitted by a StreamReader.
type EventKind int

const (
	// EventSentence is emitted exactly once for every complete sentence whose
	// checksum is valid and which has been successfully handed to the parser.
	EventSentence EventKind = iota

	// EventChecksumError is emitted for a fully framed sentence (start token,
	// '*' and two checksum hex digits) whose checksum does not match.
	// Such a sentence is never handed to the parser.
	EventChecksumError

	// EventUnsupportedSentence is emitted for a complete, checksum-valid
	// sentence whose prefix (talker ID + sentence type) has no registered
	// parser. The event error is, and unwraps to, *NotSupportedError.
	EventUnsupportedSentence

	// EventParseError is emitted for a complete, checksum-valid sentence that
	// the parser rejected for any other reason (bad field values, empty
	// prefix, etc.).
	EventParseError

	// EventMalformed is emitted for a candidate sentence that could not be
	// framed according to the start/checksum/terminator rules, for example a
	// half sentence at Flush, a line break before the checksum or a checksum
	// that is not two hexadecimal digits. Half sentences are never handed to
	// the parser.
	EventMalformed

	// EventTooLong is emitted when a candidate sentence exceeds the configured
	// maximum sentence length. The candidate buffer is released immediately.
	EventTooLong

	// EventSkipped is the explicit receipt for bytes that do not belong to any
	// sentence (garbage prefixes and other inter-sentence noise).
	EventSkipped
)

// String returns a human readable name of the event kind.
func (k EventKind) String() string {
	switch k {
	case EventSentence:
		return "sentence"
	case EventChecksumError:
		return "checksum error"
	case EventUnsupportedSentence:
		return "unsupported sentence"
	case EventParseError:
		return "parse error"
	case EventMalformed:
		return "malformed sentence"
	case EventTooLong:
		return "sentence too long"
	case EventSkipped:
		return "skipped bytes"
	default:
		return fmt.Sprintf("unknown event %d", int(k))
	}
}

// Event is emitted by StreamReader for every distinct outcome of consuming
// input bytes. Data always contains a copy of the raw bytes responsible for
// the event and may safely be retained after the callback returns:
//
//   - For EventSentence, Data holds the complete raw sentence (without
//     trailing CR/LF) and Sentence holds the parsed sentence.
//   - For EventSkipped, Data holds the skipped garbage bytes.
//   - For the error events, Data holds the raw candidate sentence (if any).
type Event struct {
	Kind     EventKind
	Data     []byte
	Sentence Sentence
	Err      error
}

// StreamHandler is the callback invoked for every event produced while reading.
type StreamHandler func(Event)

// ChecksumError attributes a fully framed sentence with a wrong checksum.
type ChecksumError struct {
	// Raw is the raw candidate sentence (without trailing CR/LF).
	Raw string
	// Got is the checksum provided in the sentence, upper-cased.
	Got string
	// Expected is the checksum computed over the sentence fields.
	Expected string
}

func (e *ChecksumError) Error() string {
	return fmt.Sprintf("nmea: sentence checksum mismatch [%s != %s]", e.Expected, e.Got)
}

// MalformedError attributes a candidate sentence that could not be framed.
type MalformedError struct {
	// Raw is the raw candidate data accumulated before framing failed.
	Raw string
	// Reason describes why the candidate is malformed.
	Reason string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf("nmea: malformed sentence %q: %s", e.Raw, e.Reason)
}

// TooLongError attributes a candidate sentence that grew past the configured
// maximum sentence length.
type TooLongError struct {
	// Raw is the raw candidate data accumulated when the limit was hit.
	Raw string
	// MaxLength is the configured maximum sentence length in bytes.
	MaxLength int
}

func (e *TooLongError) Error() string {
	return fmt.Sprintf("nmea: sentence exceeds maximum length of %d bytes: %q", e.MaxLength, e.Raw)
}

// StreamOption configures a StreamReader.
type StreamOption func(*StreamReader)

// WithMaxSentenceLength overrides the maximum raw sentence length accepted by
// the reader. Values smaller than or equal to zero fall back to
// DefaultMaxSentenceLength (82 bytes per NMEA0183).
func WithMaxSentenceLength(maxLength int) StreamOption {
	return func(r *StreamReader) {
		if maxLength <= 0 {
			maxLength = DefaultMaxSentenceLength
		}
		r.maxLength = maxLength
	}
}

// WithSentenceParser configures the SentenceParser used to turn complete,
// checksum-valid raw sentences into Sentence values. It allows using custom
// parsers, prefix/CRC hooks and the NaN/MaxInt64 options. When not provided a
// zero-value SentenceParser is used. The raw sentence handed to the parser
// always starts with '$' or '!' and ends with the two checksum digits;
// incomplete or checksum-invalid sentences never reach it.
func WithSentenceParser(parser *SentenceParser) StreamOption {
	return func(r *StreamReader) {
		if parser != nil {
			r.parser = parser
		}
	}
}

// WithStrictTerminators enables strict sentence terminator handling.
//
// Terminator policy:
//
//   - By default (lenient mode) the terminator is optional: a sentence is
//     delivered as soon as the second checksum hex digit is consumed. A
//     following CR, LF or CRLF is consumed as the optional line terminator;
//     any other byte starts inter-sentence garbage or a new sentence.
//   - In strict mode every sentence must be terminated by CRLF: the sentence
//     is only delivered after both bytes are seen. A sentence that ends
//     without CRLF is reported as EventMalformed, as is any byte that is not
//     part of the expected CRLF sequence.
func WithStrictTerminators(enabled bool) StreamOption {
	return func(r *StreamReader) {
		r.strictCRLF = enabled
	}
}

// StreamReader incrementally assembles NMEA0183 sentences from arbitrary byte
// chunks. It accepts input via Write (any chunk boundaries, including one byte
// at a time), frames sentences by their real start token ('$' or '!') and
// checksum ('*' plus two hexadecimal digits), and reports every outcome to a
// StreamHandler.
//
// Resynchronisation after garbage, a malformed candidate, a bad checksum, an
// unsupported sentence or an overlong candidate only ever resumes at an
// explicit new '$'/'!' start token, so the next well-formed sentence after an
// error is never lost. Garbage bytes are reported with an explicit
// EventSkipped receipt instead of being passed to the parser, and incomplete
// candidates (half sentences) never reach the parser.
//
// A StreamReader is not safe for concurrent use, mirroring SentenceParser.
type StreamReader struct {
	handler    StreamHandler
	parser     *SentenceParser
	maxLength  int
	strictCRLF bool

	// Framing state.
	inSentence   bool // currently accumulating a candidate starting with '$'/'!'
	starSeen     bool // '*' separator seen, checksum digits follow
	hexDigits    int  // number of checksum hex digits consumed (0..2)
	waitCR       bool // strict mode: checksum complete, waiting for CR
	waitLF       bool // strict mode: CR consumed, waiting for LF
	postSentence bool // lenient mode: sentence delivered, optional terminator may follow
	sawCR        bool // lenient mode: consumed CR of an optional CRLF

	buf  []byte // current candidate sentence
	junk []byte // garbage accumulated outside of sentences
}

// NewStreamReader returns a StreamReader that reports events to handler.
// handler may be nil, in which case events are discarded.
func NewStreamReader(handler StreamHandler, opts ...StreamOption) *StreamReader {
	r := &StreamReader{
		handler:   handler,
		parser:    &SentenceParser{},
		maxLength: DefaultMaxSentenceLength,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Write feeds a chunk of arbitrary NMEA0183 byte stream data. Every byte is
// accounted for exactly once, either as part of a sentence or through an
// EventSkipped receipt. It implements io.Writer and never returns an error.
//
// Chunk boundaries are meaningless: trailing garbage that has not been closed
// by a new sentence yet stays buffered and is reported when the next sentence
// starts, when the garbage reaches the sentence length limit, or when Flush is
// called.
func (r *StreamReader) Write(p []byte) (int, error) {
	for i := 0; i < len(p); i++ {
		r.step(p[i])
	}
	return len(p), nil
}

// Flush must be called when the input stream reaches its end. An unfinished
// candidate is reported as EventMalformed (a half sentence is never handed to
// the parser) and dangling garbage is reported as EventSkipped. In strict
// terminator mode a checksum-complete sentence missing its CRLF is reported as
// EventMalformed as well.
func (r *StreamReader) Flush() {
	if r.inSentence {
		if r.waitCR || r.waitLF {
			r.failMalformed("missing CRLF terminator at end of input")
		} else {
			r.failMalformed("incomplete sentence at end of input")
		}
	}
	r.postSentence = false
	r.sawCR = false
	r.flushJunk()
}

// BufferedLen returns the number of bytes currently buffered (candidate
// sentence plus inter-sentence garbage). It is mainly useful for asserting
// that overlong sentences do not grow the buffer unboundedly.
func (r *StreamReader) BufferedLen() int {
	return len(r.buf) + len(r.junk)
}

// step consumes a single input byte through the framing state machine.
func (r *StreamReader) step(b byte) {
	// Lenient mode: a sentence was delivered at the second checksum digit and
	// an optional line terminator (CR, LF or CRLF) may now follow.
	if r.postSentence {
		r.stepPostSentence(b)
		return
	}

	if !r.inSentence {
		if b == SentenceStart[0] || b == SentenceStartEncapsulated[0] {
			r.flushJunk()
			r.startCandidate(b)
		} else {
			r.addJunk(b)
		}
		return
	}

	// Strict mode: waiting for the mandatory CRLF terminator.
	if r.waitCR || r.waitLF {
		r.stepStrictTerminator(b)
		return
	}

	// A line break before the checksum is complete aborts the candidate.
	if b == '\r' || b == '\n' {
		r.abortMalformed("unexpected line terminator before checksum", b)
		return
	}

	// An explicit new start token abandons the current candidate and starts a
	// fresh resynchronisation point, even while the candidate is growing past
	// its length limit.
	if b == SentenceStart[0] || b == SentenceStartEncapsulated[0] {
		r.abortMalformed("unexpected new sentence start before checksum", b)
		return
	}

	// Enforce the length cap before buffering anything else so an overlong
	// candidate can never grow past the limit. The run that exceeded the limit
	// is discarded and its remaining bytes are accounted as garbage;
	// resynchronisation only resumes at an explicit '$'/'!' start token (handled
	// above).
	if len(r.buf) >= r.maxLength {
		data := r.copyBuf()
		r.resetCandidate()
		r.emit(Event{Kind: EventTooLong, Data: data, Err: &TooLongError{Raw: string(data), MaxLength: r.maxLength}})
		r.addJunk(b)
		return
	}

	if !r.starSeen {
		// Only printable ASCII is allowed in the sentence body. Control bytes
		// and non-ASCII bytes cannot belong to a valid framed sentence.
		if b < 0x20 || b > 0x7E {
			r.abortMalformed("non-printable byte in sentence body", b)
			return
		}
		r.buf = append(r.buf, b)
		if b == ChecksumSep[0] {
			r.starSeen = true
		}
		return
	}

	// After '*' exactly two hexadecimal checksum digits are allowed.
	if r.hexDigits < 2 && isHexDigit(b) {
		r.buf = append(r.buf, b)
		r.hexDigits++
		if r.hexDigits == 2 {
			if r.strictCRLF {
				r.waitCR = true
			} else {
				r.deliver()
			}
		}
		return
	}

	r.abortMalformed("checksum must be two hexadecimal digits", b)
}

// stepPostSentence consumes bytes following a delivered sentence in lenient
// mode and accepts an optional CR, LF or CRLF terminator.
func (r *StreamReader) stepPostSentence(b byte) {
	if !r.sawCR {
		switch b {
		case '\r':
			r.sawCR = true
			return
		case '\n':
			r.endPostSentence()
			return
		default:
			// No terminator: reprocess the byte in the looking state.
			r.endPostSentence()
			r.step(b)
			return
		}
	}
	if b == '\n' {
		r.endPostSentence()
		return
	}
	// The earlier CR was not part of a CRLF: account for it as garbage and
	// reprocess the current byte.
	r.endPostSentence()
	r.addJunk('\r')
	r.step(b)
}

func (r *StreamReader) endPostSentence() {
	r.postSentence = false
	r.sawCR = false
}

// stepStrictTerminator enforces the mandatory CRLF in strict terminator mode.
func (r *StreamReader) stepStrictTerminator(b byte) {
	if r.waitCR {
		if b == '\r' {
			r.waitCR = false
			r.waitLF = true
			return
		}
		r.abortMalformed("expected CR after checksum", b)
		return
	}
	if b == '\n' {
		r.waitLF = false
		r.deliver()
		return
	}
	r.abortMalformed("expected LF after CR", b)
}

// startCandidate begins a new candidate sentence at an explicit start token.
func (r *StreamReader) startCandidate(b byte) {
	r.inSentence = true
	r.starSeen = false
	r.hexDigits = 0
	r.waitCR = false
	r.waitLF = false
	r.buf = r.buf[:0]
	r.buf = append(r.buf, b)
}

// resetCandidate drops all candidate framing state. The backing buffer is kept
// for reuse; emitted events always carry copies.
func (r *StreamReader) resetCandidate() {
	r.inSentence = false
	r.starSeen = false
	r.hexDigits = 0
	r.waitCR = false
	r.waitLF = false
	r.buf = r.buf[:0]
}

// abortMalformed reports the current candidate as malformed and reprocesses
// the offending byte so resynchronisation can continue from an explicit new
// start token (or accumulate as skipped garbage).
func (r *StreamReader) abortMalformed(reason string, b byte) {
	r.failMalformed(reason)
	r.step(b)
}

// failMalformed reports the current candidate as malformed without
// reprocessing any byte (used by Flush).
func (r *StreamReader) failMalformed(reason string) {
	data := r.copyBuf()
	r.resetCandidate()
	if len(data) == 0 {
		data = nil
	}
	r.emit(Event{
		Kind: EventMalformed,
		Data: data,
		Err:  &MalformedError{Raw: string(data), Reason: reason},
	})
}

// deliver validates the checksum of the fully framed candidate and hands it to
// the parser. The candidate is reported exactly once, then framing restarts.
func (r *StreamReader) deliver() {
	raw := r.copyBuf()
	r.resetCandidate()

	star := -1
	for i := 1; i < len(raw); i++ {
		if raw[i] == ChecksumSep[0] {
			star = i
			break
		}
	}
	// star is always present and there are exactly two hex digits here.
	body := raw[1:star]
	got := strings.ToUpper(string(raw[star+1:]))
	expected := Checksum(string(body))
	if expected != got {
		r.emit(Event{
			Kind: EventChecksumError,
			Data: raw,
			Err:  &ChecksumError{Raw: string(raw), Got: got, Expected: expected},
		})
		return
	}

	sentence, err := r.parser.Parse(string(raw))
	if err != nil {
		var notSupported *NotSupportedError
		kind := EventParseError
		if errors.As(err, &notSupported) {
			kind = EventUnsupportedSentence
		}
		r.emit(Event{Kind: kind, Data: raw, Err: err})
		return
	}

	r.emit(Event{Kind: EventSentence, Data: raw, Sentence: sentence})
	if !r.strictCRLF {
		// Lenient mode: the (optional) CR/LF terminator may arrive next.
		r.postSentence = true
	}
}

// addJunk appends a garbage byte, flushing a skip receipt first if the garbage
// buffer is already at the sentence length limit so the internal buffer can
// never grow unboundedly.
func (r *StreamReader) addJunk(b byte) {
	if len(r.junk) >= r.maxLength {
		r.flushJunk()
	}
	r.junk = append(r.junk, b)
}

// flushJunk emits a skip receipt for accumulated garbage, if any.
func (r *StreamReader) flushJunk() {
	if len(r.junk) == 0 {
		return
	}
	data := make([]byte, len(r.junk))
	copy(data, r.junk)
	r.junk = r.junk[:0]
	r.emit(Event{Kind: EventSkipped, Data: data})
}

// copyBuf returns a copy of the current candidate bytes.
func (r *StreamReader) copyBuf() []byte {
	data := make([]byte, len(r.buf))
	copy(data, r.buf)
	return data
}

// emit dispatches an event to the handler, if set.
func (r *StreamReader) emit(e Event) {
	if r.handler != nil {
		r.handler(e)
	}
}

// isHexDigit reports whether b is an ASCII hexadecimal digit.
func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'A' && b <= 'F') || (b >= 'a' && b <= 'f')
}
