package nmea

import "errors"

// DiagnosticErrorKind classifies the stage at which sentence parsing failed
// in diagnostic mode.
type DiagnosticErrorKind int

const (
	// DiagnosticFormatError indicates the sentence is malformed: it does not
	// start with '$' or '!', has an empty prefix, an invalid tag block, etc.
	DiagnosticFormatError DiagnosticErrorKind = iota

	// DiagnosticChecksumError indicates the sentence header and fields were
	// decoded but checksum validation failed.
	DiagnosticChecksumError
)

// String returns a human readable name of the diagnostic error kind.
func (k DiagnosticErrorKind) String() string {
	switch k {
	case DiagnosticChecksumError:
		return "checksum"
	default:
		return "format"
	}
}

// ChecksumError is returned when checksum validation of a sentence fails.
// It carries the partially parsed BaseSentence (original talker, sentence
// type and raw fields) for diagnostic purposes and wraps the original
// checksum validation error.
type ChecksumError struct {
	Sentence BaseSentence
	Err      error
}

// Error returns the underlying checksum validation error message.
func (e *ChecksumError) Error() string { return e.Err.Error() }

// Unwrap returns the original checksum validation error.
func (e *ChecksumError) Unwrap() error { return e.Err }

// DiagnosticError classifies a parse failure observed in diagnostic mode.
// The original error chain is preserved and can be inspected with
// errors.Unwrap / errors.As / errors.Is.
type DiagnosticError struct {
	Kind DiagnosticErrorKind
	Err  error
}

// Error returns the underlying parse error message.
func (e *DiagnosticError) Error() string { return e.Err.Error() }

// Unwrap returns the original parse error.
func (e *DiagnosticError) Unwrap() error { return e.Err }

// SentenceDiagnostic is a read-only diagnostic view of a raw NMEA sentence.
//
// Unlike Parse, Diagnose never produces a trusted Sentence (e.g. a
// positioning object). Even when checksum validation fails the diagnostic
// exposes the original talker, sentence type and raw fields so the offending
// sentence can be logged or inspected.
type SentenceDiagnostic struct {
	Talker   string   // The talker id (e.g GP), empty when the header is malformed
	Type     string   // The sentence type (e.g RMC), empty when the header is malformed
	Fields   []string // The raw data fields (without the address field)
	Checksum string   // The raw checksum as received
	Raw      string   // The raw sentence (tag block excluded)
}

// Prefix returns the talker and type of the diagnosed sentence.
func (d SentenceDiagnostic) Prefix() string { return d.Talker + d.Type }

// Diagnose parses the given raw sentence in read-only diagnostic mode using
// the default sentence parser. See SentenceParser.Diagnose for details.
func Diagnose(raw string) (SentenceDiagnostic, error) {
	defaultSentenceParserMu.Lock()
	defer defaultSentenceParserMu.Unlock()

	return defaultSentenceParser.Diagnose(raw)
}

// Diagnose parses the given raw sentence in read-only diagnostic mode.
//
// The returned SentenceDiagnostic always reflects what could be decoded from
// the raw input and never yields a trusted, fully parsed Sentence:
//
//   - On success the diagnostic is returned with a nil error.
//   - When checksum validation fails, the diagnostic still contains the
//     original talker, sentence type and raw fields, and the returned error
//     is a *DiagnosticError with Kind == DiagnosticChecksumError wrapping a
//     *ChecksumError (which in turn wraps the original checksum error).
//   - When the sentence is malformed (bad header, empty prefix, ...), the
//     returned error is a *DiagnosticError with Kind == DiagnosticFormatError
//     and the diagnostic only contains the parts that could be decoded.
//
// Diagnose does not change the behaviour of Parse in any way; callers that
// do not use this entry point keep using the original interface unchanged.
func (p *SentenceParser) Diagnose(raw string) (SentenceDiagnostic, error) {
	s, err := p.parseBaseSentenceEx(raw)
	diag := SentenceDiagnostic{
		Talker:   s.Talker,
		Type:     s.Type,
		Fields:   append([]string(nil), s.Fields...),
		Checksum: s.Checksum,
		Raw:      s.Raw,
	}
	if err == nil {
		return diag, nil
	}
	var csErr *ChecksumError
	if errors.As(err, &csErr) {
		return diag, &DiagnosticError{Kind: DiagnosticChecksumError, Err: err}
	}
	return diag, &DiagnosticError{Kind: DiagnosticFormatError, Err: err}
}
