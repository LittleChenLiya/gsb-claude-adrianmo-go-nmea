package nmea

import (
	"errors"
	"fmt"
	"strings"
)

// DefaultMaxSentenceLength is the default maximum number of bytes a single
// sentence may occupy in the Streamer buffer before it is rejected with a
// TooLongError. The NMEA 0183 specification limits sentences to 82 characters,
// but some proprietary and encapsulated sentences are longer, so the default
// is lenient. The limit keeps the internal buffer bounded even when the
// terminator of a sentence is never received.
const DefaultMaxSentenceLength = 1024

// TerminatorPolicy defines which line ending the Streamer accepts as the end
// of a sentence.
type TerminatorPolicy int

const (
	// TerminatorAny (default) accepts a sentence terminated by LF ("\n") or
	// CRLF ("\r\n"); a single trailing '\r' before the '\n' is stripped.
	TerminatorAny TerminatorPolicy = iota
	// TerminatorCRLF strictly accepts only CRLF ("\r\n") as the sentence
	// terminator. A bare '\n' does not terminate a sentence; bytes up to the
	// next explicit sentence start are discarded with a skip/error receipt.
	TerminatorCRLF
	// TerminatorLF accepts only LF ("\n") as the sentence terminator and does
	// not strip a preceding '\r' (it becomes part of the raw sentence).
	TerminatorLF
)

// String returns a human readable name of the terminator policy.
func (t TerminatorPolicy) String() string {
	switch t {
	case TerminatorCRLF:
		return "CRLF"
	case TerminatorLF:
		return "LF"
	default:
		return "Any"
	}
}

// ChecksumError is reported by the Streamer when a complete, terminated
// sentence carries a missing or mismatching checksum. It is distinct from
// NotSupportedError so callers can attribute framing/checksum failures
// separately from unsupported talkers/sentence types.
type ChecksumError struct {
	Raw      string // the complete raw sentence line (without terminator)
	Expected string // checksum computed over the sentence fields
	Actual   string // checksum found in the sentence ("" when missing)
}

// Error returns the error message.
func (e *ChecksumError) Error() string {
	if e.Actual == "" {
		return fmt.Sprintf("nmea: stream: sentence checksum missing [%s expected %s]", e.Raw, e.Expected)
	}
	return fmt.Sprintf("nmea: stream: sentence checksum mismatch [%s != %s]", e.Expected, e.Actual)
}

// UnterminatedError is reported when a new sentence start ('$', '!' or a tag
// block) is seen before the current sentence was terminated. The partial
// sentence is discarded and the Streamer resynchronizes from the explicit new
// start, so the following sentence is not lost.
type UnterminatedError struct {
	Raw string // the discarded partial sentence
}

// Error returns the error message.
func (e *UnterminatedError) Error() string {
	return fmt.Sprintf("nmea: stream: unterminated sentence discarded: %q", e.Raw)
}

// TooLongError is reported when a sentence exceeds the configured maximum
// length without being terminated. The partial sentence is dropped and the
// internal buffer is released, so an unterminated sentence cannot grow the
// buffer without bound.
type TooLongError struct {
	Limit int    // the configured maximum sentence length
	Raw   string // prefix of the discarded sentence (may be truncated)
}

// Error returns the error message.
func (e *TooLongError) Error() string {
	return fmt.Sprintf("nmea: stream: sentence exceeds maximum length of %d bytes", e.Limit)
}

// StreamerOption is a functional option for configuring a Streamer.
type StreamerOption func(*Streamer)

// WithStreamParser configures the SentenceParser used to parse complete
// sentences. When not set, the package level Parse function is used.
func WithStreamParser(p *SentenceParser) StreamerOption {
	return func(s *Streamer) {
		if p != nil {
			s.parser = p
		}
	}
}

// WithMaxSentenceLength sets the maximum number of bytes a single sentence may
// occupy before it is rejected with a TooLongError. Values <= 0 select
// DefaultMaxSentenceLength.
func WithMaxSentenceLength(n int) StreamerOption {
	return func(s *Streamer) {
		if n > 0 {
			s.maxLen = n
		}
	}
}

// WithTerminator sets the accepted sentence terminator policy. The default is
// TerminatorAny (LF or CRLF).
func WithTerminator(t TerminatorPolicy) StreamerOption {
	return func(s *Streamer) {
		s.terminator = t
	}
}

// WithOnSentence registers the callback invoked exactly once for every
// complete, checksum-valid sentence. Only complete sentences are handed to
// the parser; partial sentences are never delivered.
func WithOnSentence(fn func(s Sentence)) StreamerOption {
	return func(s *Streamer) { s.onSentence = fn }
}

// WithOnSkip registers the callback invoked with the exact bytes that were
// discarded as garbage (bytes that do not belong to any sentence, e.g. a
// garbage prefix before the first sentence start).
func WithOnSkip(fn func(garbage []byte)) StreamerOption {
	return func(s *Streamer) { s.onSkip = fn }
}

// WithOnError registers the callback invoked when a started sentence cannot
// be delivered. err is one of *ChecksumError, *UnterminatedError,
// *TooLongError, *NotSupportedError (unsupported talker/sentence type) or a
// parse error returned by the underlying parser. raw is the raw sentence line
// the error belongs to (may be truncated for TooLongError).
func WithOnError(fn func(err error, raw string)) StreamerOption {
	return func(s *Streamer) { s.onError = fn }
}

// Streamer incrementally assembles NMEA 0183 sentences from arbitrary byte
// chunks. Bytes may be fed in any size (including one byte at a time); a
// sentence is delivered exactly once, and only after its terminator and a
// valid checksum have been seen. Garbage between sentences is reported via
// the skip callback, checksum failures and unsupported talkers are reported
// with distinct error types, and resynchronization after an error always
// happens at an explicit new sentence start so the next valid sentence is
// never lost.
//
// Streamer is not co-routine safe.
type Streamer struct {
	parser     *SentenceParser
	maxLen     int
	terminator TerminatorPolicy

	onSentence func(s Sentence)
	onSkip     func(garbage []byte)
	onError    func(err error, raw string)

	buf []byte
}

// NewStreamer constructs a Streamer with the given options.
func NewStreamer(opts ...StreamerOption) *Streamer {
	s := &Streamer{maxLen: DefaultMaxSentenceLength}
	for _, opt := range opts {
		opt(s)
	}
	if s.maxLen <= 0 {
		s.maxLen = DefaultMaxSentenceLength
	}
	return s
}

// Buffered returns the number of bytes currently held in the internal buffer.
// It is bounded by the configured maximum sentence length plus the size of
// the last fed chunk.
func (s *Streamer) Buffered() int { return len(s.buf) }

// Feed consumes a chunk of bytes. Complete sentences, skip receipts and
// errors are reported through the configured callbacks, synchronously, before
// Feed returns.
func (s *Streamer) Feed(p []byte) {
	s.buf = append(s.buf, p...)
	s.process()
}

// isStartByte reports whether b can explicitly start a new sentence: '$' or
// '!' for the sentence itself, '\' for a leading tag block.
func isStartByte(b byte) bool {
	return b == SentenceStart[0] || b == SentenceStartEncapsulated[0] || b == TagBlockSep
}

// indexStart returns the index of the first sentence start byte at or after
// from, or -1 when there is none.
func indexStart(buf []byte, from int) int {
	for i := from; i < len(buf); i++ {
		if isStartByte(buf[i]) {
			return i
		}
	}
	return -1
}

// findTerminator locates the sentence terminator in buf according to the
// configured policy. It returns the index of the terminator and the
// terminator length in bytes. end is -1 when no terminator is present yet.
func (s *Streamer) findTerminator(buf []byte) (end, termLen int) {
	switch s.terminator {
	case TerminatorCRLF:
		if i := strings.Index(string(buf), "\r\n"); i != -1 {
			return i, 2
		}
	case TerminatorLF:
		if i := strings.IndexByte(string(buf), '\n'); i != -1 {
			return i, 1
		}
	default: // TerminatorAny
		if i := strings.IndexByte(string(buf), '\n'); i != -1 {
			if i > 0 && buf[i-1] == '\r' {
				return i - 1, 2
			}
			return i, 1
		}
	}
	return -1, 0
}

func (s *Streamer) process() {
	for {
		// Discard garbage preceding the next explicit sentence start.
		i := indexStart(s.buf, 0)
		if i == -1 {
			if len(s.buf) > 0 {
				s.emitSkip(s.buf)
				s.buf = s.buf[:0]
			}
			return
		}
		if i > 0 {
			s.emitSkip(s.buf[:i])
			s.buf = s.buf[i:]
		}
		// s.buf[0] is now an explicit sentence start.
		end, termLen := s.findTerminator(s.buf)
		// A new start before the terminator aborts the current partial
		// sentence; resynchronize from the explicit new start so the next
		// sentence is not lost. For lines starting with a tag block, the
		// closing '\' of the tag block and the sentence start directly
		// following it belong to the current line and are not resync points.
		searchFrom := 1
		if s.buf[0] == TagBlockSep {
			if c := strings.IndexByte(string(s.buf[1:]), TagBlockSep); c != -1 {
				searchFrom = c + 2
				if searchFrom < len(s.buf) && (s.buf[searchFrom] == SentenceStart[0] || s.buf[searchFrom] == SentenceStartEncapsulated[0]) {
					searchFrom++
				}
			}
		}
		if j := indexStart(s.buf, searchFrom); j != -1 && (end == -1 || j < end) {
			s.emitError(&UnterminatedError{Raw: string(s.buf[:j])})
			s.buf = s.buf[j:]
			continue
		}
		if end == -1 {
			if len(s.buf) > s.maxLen {
				raw := string(s.buf)
				if len(raw) > 64 {
					raw = raw[:64] + "..."
				}
				s.emitError(&TooLongError{Limit: s.maxLen, Raw: raw})
				// Drop the oversized partial sentence and resynchronize at
				// the next explicit start, if any, so the buffer cannot grow
				// without bound.
				if j := indexStart(s.buf, 1); j != -1 {
					s.buf = s.buf[j:]
					continue
				}
				s.buf = s.buf[:0]
			}
			return
		}
		line := string(s.buf[:end])
		s.buf = s.buf[end+termLen:]
		s.handleLine(line)
	}
}

// handleLine validates and parses one complete, terminated sentence line.
func (s *Streamer) handleLine(line string) {
	sentPart := line
	if strings.HasPrefix(line, string(TagBlockSep)) {
		// A leading tag block is delimited by a second '\'.
		closing := strings.IndexByte(line[1:], TagBlockSep)
		if closing == -1 {
			s.emitError(errors.New("nmea: stream: unterminated tag block"), line)
			return
		}
		sentPart = line[closing+2:]
	}
	if sentPart == "" || (sentPart[0] != SentenceStart[0] && sentPart[0] != SentenceStartEncapsulated[0]) {
		s.emitError(errors.New("nmea: stream: sentence does not start with a '$' or '!'"), line)
		return
	}
	// Attribute checksum failures before handing anything to the parser.
	star := strings.Index(sentPart, ChecksumSep)
	if star == -1 {
		s.emitError(&ChecksumError{Raw: line, Expected: Checksum(sentPart[1:])}, line)
		return
	}
	expected := Checksum(sentPart[1:star])
	actual := strings.ToUpper(sentPart[star+1:])
	if expected != actual {
		s.emitError(&ChecksumError{Raw: line, Expected: expected, Actual: actual}, line)
		return
	}
	// Only complete, checksum-valid sentences reach the parser.
	sent, err := s.parse(line)
	if err != nil {
		s.emitError(err, line)
		return
	}
	if s.onSentence != nil {
		s.onSentence(sent)
	}
}

func (s *Streamer) parse(raw string) (Sentence, error) {
	if s.parser != nil {
		return s.parser.Parse(raw)
	}
	return Parse(raw)
}

func (s *Streamer) emitSkip(garbage []byte) {
	if s.onSkip != nil && len(garbage) > 0 {
		cp := make([]byte, len(garbage))
		copy(cp, garbage)
		s.onSkip(cp)
	}
}

func (s *Streamer) emitError(err error, raw ...string) {
	if s.onError != nil {
		r := ""
		if len(raw) > 0 {
			r = raw[0]
		}
		s.onError(err, r)
	}
}
