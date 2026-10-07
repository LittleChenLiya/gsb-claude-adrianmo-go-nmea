package nmea

import (
	"errors"
)

// Sentinel errors used to distinguish the two failure classes.
var (
	// ErrFormat is matched by errors.Is for sentence format/structural failures
	// (e.g. missing "$"/"!" header, invalid address prefix, missing checksum
	// separator, malformed tag block). The original failure is wrapped and kept
	// in the error chain via Unwrap.
	ErrFormat = errors.New("nmea: sentence format error")

	// ErrChecksum is matched by errors.Is for checksum verification failures.
	// The original failure is wrapped and kept in the error chain via Unwrap.
	ErrChecksum = errors.New("nmea: sentence checksum error")
)

// FormatError describes a sentence format/structural failure. It wraps the
// original parser error (available through Unwrap/errors.Is/errors.As) and may
// carry the partially recovered base sentence (Raw is always populated; the
// remaining fields are available only if parsing reached that stage).
type FormatError struct {
	// Sentence contains the partially recovered base sentence. Its fields must
	// not be treated as trusted data.
	Sentence BaseSentence
	// Err is the original error produced while parsing.
	Err error
}

// Error implements the error interface.
func (e *FormatError) Error() string {
	if e == nil || e.Err == nil {
		return ErrFormat.Error()
	}
	return ErrFormat.Error() + ": " + e.Err.Error()
}

// Unwrap exposes the original error so the error chain is preserved.
func (e *FormatError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is enables errors.Is(err, ErrFormat).
func (e *FormatError) Is(target error) bool {
	return target == ErrFormat
}

// ChecksumError describes a checksum verification failure. It wraps the
// original checksum error (available through Unwrap/errors.Is/errors.As) and
// carries the unverified base sentence holding the original talker id,
// sentence type and raw fields. The data in Sentence is untrusted and must not
// be used to build positioning/navigation objects.
type ChecksumError struct {
	// Expected is the checksum that was locally computed from rawFields.
	Expected string
	// Actual is the checksum value carried by the received sentence.
	//
	// The field names follow the wording of the original checksum error message
	// "checksum mismatch [computed != received]": Expected holds the locally
	// computed value, Actual the value received in the sentence.
	Actual string
	// Sentence contains the unverified base sentence recovered from the raw
	// input. Its fields must not be treated as trusted data.
	Sentence BaseSentence
	// Err is the original checksum error produced while verifying.
	Err error
}

// Error implements the error interface.
func (e *ChecksumError) Error() string {
	if e == nil || e.Err == nil {
		return ErrChecksum.Error()
	}
	return ErrChecksum.Error() + ": " + e.Err.Error()
}

// Unwrap exposes the original error so the error chain is preserved.
func (e *ChecksumError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Is enables errors.Is(err, ErrChecksum).
func (e *ChecksumError) Is(target error) bool {
	return target == ErrChecksum
}

// Diagnostic is the read-only result of a diagnostic parse attempt. It only
// exposes raw sentence metadata and never a typed/trusted Sentence such as a
// GGA or RMC positioning object.
type Diagnostic struct {
	// Kind is the outcome class of the attempt.
	Kind DiagnosticKind
	// Talker is the original talker id recovered from the address field
	// (e.g. "GP"). It is empty when the sentence header could not be parsed.
	Talker string
	// Type is the original sentence type recovered from the address field
	// (e.g. "GGA"). It is empty when the address could not be parsed.
	Type string
	// Fields are the original raw data fields exactly as received (the address
	// field is excluded). They are unverified on checksum failure.
	Fields []string
	// Checksum is the checksum value carried by the sentence, if present.
	Checksum string
	// Raw is the original (trimmed) NMEA sentence.
	Raw string
	// TagBlock holds the parsed tag block when the sentence contained one.
	TagBlock TagBlock
	// Err is the failure, nil for DiagnosticOK. It is either a *FormatError
	// or a *ChecksumError and always keeps the original parser error in its
	// chain.
	Err error
}

// DiagnosticKind classifies the outcome of a diagnostic parse attempt.
type DiagnosticKind int

const (
	// DiagnosticUnknown is the zero value and must never be returned by
	// Diagnose.
	DiagnosticUnknown DiagnosticKind = iota
	// DiagnosticOK means the sentence was structurally valid and its checksum
	// verified successfully; no trusted Sentence is produced by Diagnose.
	DiagnosticOK
	// DiagnosticChecksumError means a structurally parseable sentence failed
	// checksum verification; Talker/Type/Fields are populated but untrusted.
	DiagnosticChecksumError
	// DiagnosticFormatError means the sentence was malformed at the format
	// level (header/delimiter/address/tag block); recovered fields may be empty.
	DiagnosticFormatError
)

// String returns a human readable label for the diagnostic kind.
func (k DiagnosticKind) String() string {
	switch k {
	case DiagnosticOK:
		return "ok"
	case DiagnosticChecksumError:
		return "checksum-error"
	case DiagnosticFormatError:
		return "format-error"
	default:
		return "unknown"
	}
}

// TrustedSentence reports whether a trusted, typed Sentence (e.g. a
// positioning object) is available. It is always false: Diagnose is a
// read-only diagnostic entry point and never produces trusted objects.
func (d Diagnostic) TrustedSentence() bool {
	return false
}

// Prefix returns the recovered talker id plus sentence type.
func (d Diagnostic) Prefix() string {
	return d.Talker + d.Type
}

// cloneBaseSentence returns a copy of s with a dedicated backing array for
// Fields so callers cannot mutate parser state through diagnostic results.
func cloneBaseSentence(s BaseSentence) BaseSentence {
	if s.Fields != nil {
		fields := make([]string, len(s.Fields))
		copy(fields, s.Fields)
		s.Fields = fields
	}
	return s
}

// diagnosticFromSentence builds a Diagnostic from a recovered base sentence,
// using the full (trimmed) original input as Raw.
func diagnosticFromSentence(s BaseSentence, raw string, kind DiagnosticKind, err error) Diagnostic {
	s = cloneBaseSentence(s)
	s.Raw = raw
	return Diagnostic{
		Kind:     kind,
		Talker:   s.Talker,
		Type:     s.Type,
		Fields:   s.Fields,
		Checksum: s.Checksum,
		Raw:      s.Raw,
		TagBlock: s.TagBlock,
		Err:      err,
	}
}

// Diagnose performs a read-only, non-trusting parse of raw.
//
// Unlike Parse it never returns a typed Sentence/positioning object. On a
// checksum failure it returns the original talker id, sentence type and raw
// fields together with a *ChecksumError (matching ErrChecksum); format errors
// are reported separately as *FormatError (matching ErrFormat). Both error
// types preserve the original parser error in their chain (errors.Is/As keep
// working). Normal Parse behavior is not affected by calling Diagnose.
//
// This method is not co-routine safe.
func (p *SentenceParser) Diagnose(raw string) Diagnostic {
	original := trimSpace(raw)
	s, rawFields, crcFailed, err := p.parseBaseSentenceDetailed(raw)
	if err == nil {
		return diagnosticFromSentence(s, original, DiagnosticOK, nil)
	}
	if crcFailed && s.Checksum != "" {
		cErr := &ChecksumError{
			Expected: Checksum(rawFields),
			Actual:   s.Checksum,
			Sentence: cloneBaseSentence(s),
			Err:      err,
		}
		return diagnosticFromSentence(s, original, DiagnosticChecksumError, cErr)
	}
	fErr := &FormatError{
		Sentence: cloneBaseSentence(BaseSentence{
			Talker:   s.Talker,
			Type:     s.Type,
			Fields:   s.Fields,
			Checksum: s.Checksum,
			Raw:      original,
			TagBlock: s.TagBlock,
		}),
		Err: err,
	}
	return diagnosticFromSentence(s, original, DiagnosticFormatError, fErr)
}

// trimSpace removes leading/trailing ASCII whitespace, mirroring
// strings.TrimSpace used by the regular parser.
func trimSpace(s string) string {
	isSpace := func(b byte) bool {
		switch b {
		case ' ', '\t', '\r', '\n':
			return true
		}
		return false
	}
	for len(s) > 0 && isSpace(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// Diagnose is the package level, read-only diagnostic entry point backed by
// the default parser. It exists alongside Parse; Parse keeps its historical
// behavior. The returned Diagnostic never carries a trusted Sentence.
func Diagnose(raw string) Diagnostic {
	defaultSentenceParserMu.Lock()
	defer defaultSentenceParserMu.Unlock()

	return defaultSentenceParser.Diagnose(raw)
}
