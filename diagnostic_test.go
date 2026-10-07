package nmea

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// In-memory fixtures: the same GGA sentence in three states.
const (
	goodGGA = "$GNGGA,203415.000,6325.6138,N,01021.4290,E,1,8,2.42,72.5,M,41.5,M,,*7C"
	// same sentence body with a tampered checksum
	badChecksumGGA = "$GNGGA,203415.000,6325.6138,N,01021.4290,E,1,8,2.42,72.5,M,41.5,M,,*00"
	// malformed header: no '$'/'!' start delimiter
	malformedGGA = "%GNGGA,203415.000,6325.6138,N,01021.4290,E,1,8,2.42,72.5,M,41.5,M,,*7C"
)

var expectedRawFields = []string{
	"203415.000", "6325.6138", "N", "01021.4290", "E", "1",
	"8", "2.42", "72.5", "M", "41.5", "M", "", "",
}

// TestDiagnoseThreeSamples covers the acceptance check: the same sentence in
// its valid, bad-checksum and malformed-header forms, asserting diagnostic
// fields, the error class/type and that no trusted positioning result can be
// obtained.
func TestDiagnoseThreeSamples(t *testing.T) {
	t.Run("valid sentence: ok, still no trusted object", func(t *testing.T) {
		d := Diagnose(goodGGA)

		assert.Equal(t, DiagnosticOK, d.Kind)
		assert.Equal(t, "GN", d.Talker)
		assert.Equal(t, "GGA", d.Type)
		assert.Equal(t, "GNGGA", d.Prefix())
		assert.Equal(t, "7C", d.Checksum)
		assert.Equal(t, goodGGA, d.Raw)
		assert.Equal(t, expectedRawFields, d.Fields)
		assert.NoError(t, d.Err)
		assert.False(t, d.TrustedSentence(), "diagnostic must never expose a trusted Sentence")

		// Diagnose must not hand back a typed positioning object: it has no
		// accessor returning one at all. Cross-check that Parse does produce it.
		s, err := Parse(goodGGA)
		require.NoError(t, err)
		gga, ok := s.(GGA)
		require.True(t, ok)
		assert.Equal(t, 72.5, gga.Altitude)
	})

	t.Run("bad checksum: original talker/type/fields preserved, untrusted", func(t *testing.T) {
		d := Diagnose(badChecksumGGA)

		assert.Equal(t, DiagnosticChecksumError, d.Kind)
		assert.Equal(t, "GN", d.Talker)
		assert.Equal(t, "GGA", d.Type)
		assert.Equal(t, "GNGGA", d.Prefix())
		assert.Equal(t, "00", d.Checksum)
		assert.Equal(t, badChecksumGGA, d.Raw)
		assert.Equal(t, expectedRawFields, d.Fields, "original raw fields must be returned verbatim")
		assert.False(t, d.TrustedSentence())

		var cErr *ChecksumError
		require.Error(t, d.Err)
		assert.True(t, errors.As(d.Err, &cErr))
		assert.True(t, errors.Is(d.Err, ErrChecksum))
		assert.False(t, errors.Is(d.Err, ErrFormat), "checksum and format errors must be separated")
		assert.Equal(t, "7C", cErr.Expected, "locally computed checksum")
		assert.Equal(t, "00", cErr.Actual, "checksum carried by the sentence")
		assert.Equal(t, "GN", cErr.Sentence.Talker)
		assert.Equal(t, "GGA", cErr.Sentence.Type)
		assert.Equal(t, expectedRawFields, cErr.Sentence.Fields)

		// Original error chain must be preserved.
		assert.True(t,
			strings.Contains(cErr.Unwrap().Error(), "nmea: sentence checksum mismatch [7C != 00]"),
			"original error must remain in the chain, got %v", cErr.Unwrap())
		assert.Contains(t, d.Err.Error(), "nmea: sentence checksum mismatch [7C != 00]")
	})

	t.Run("malformed header: format error, no talker/type/fields", func(t *testing.T) {
		d := Diagnose(malformedGGA)

		assert.Equal(t, DiagnosticFormatError, d.Kind)
		assert.Equal(t, "", d.Talker)
		assert.Equal(t, "", d.Type)
		assert.Equal(t, "", d.Prefix())
		assert.Nil(t, d.Fields)
		assert.Equal(t, malformedGGA, d.Raw)
		assert.False(t, d.TrustedSentence())

		var fErr *FormatError
		require.Error(t, d.Err)
		assert.True(t, errors.As(d.Err, &fErr))
		assert.True(t, errors.Is(d.Err, ErrFormat))
		assert.False(t, errors.Is(d.Err, ErrChecksum), "format error must not be reported as checksum")
		assert.Equal(t, "nmea: sentence does not start with a '$' or '!'",
			fErr.Unwrap().Error())
		assert.Contains(t, d.Err.Error(), "nmea: sentence does not start with a '$' or '!'")
	})
}

// TestDiagnoseErrorClassification checks additional boundary cases of the
// format/checksum split.
func TestDiagnoseErrorClassification(t *testing.T) {
	t.Run("missing checksum separator is a format error", func(t *testing.T) {
		d := Diagnose("$GPFOO,1,2,3,x,y,z")
		assert.Equal(t, DiagnosticFormatError, d.Kind)
		assert.True(t, errors.Is(d.Err, ErrFormat))
		assert.False(t, errors.Is(d.Err, ErrChecksum))
		assert.Contains(t, d.Err.Error(), "does not contain checksum separator")
	})

	t.Run("empty address prefix is a format error with raw kept", func(t *testing.T) {
		raw := "$,1,2,3,x,y,z*4B"
		d := Diagnose(raw)
		assert.Equal(t, DiagnosticFormatError, d.Kind)
		assert.True(t, errors.Is(d.Err, ErrFormat))
		assert.Equal(t, raw, d.Raw)
		assert.Contains(t, d.Err.Error(), "prefix is empty")
	})

	t.Run("checksum failure on RMC preserves original fields", func(t *testing.T) {
		// valid original ends with *0C; tamper it to *0A
		raw := "$GPRMC,235236,A,3925.9479,N,11945.9211,W,44.7,153.6,250905,15.2,E,A*0A"
		d := Diagnose(raw)
		assert.Equal(t, DiagnosticChecksumError, d.Kind)
		assert.Equal(t, "GP", d.Talker)
		assert.Equal(t, "RMC", d.Type)
		assert.Equal(t, "235236", d.Fields[0])
		assert.Equal(t, "A", d.Fields[1])
		var cErr *ChecksumError
		require.True(t, errors.As(d.Err, &cErr))
		assert.Equal(t, "0C", cErr.Expected)
		assert.Equal(t, "0A", cErr.Actual)
	})
}

// TestDiagnoseIsReadOnly verifies the diagnostic result cannot mutate parser
// state or serve as a positioning object, and that the default Parse path is
// untouched.
func TestDiagnoseIsReadOnly(t *testing.T) {
	d := Diagnose(badChecksumGGA)
	require.Equal(t, DiagnosticChecksumError, d.Kind)

	// Mutating returned fields must not affect a subsequent diagnostic run.
	d.Fields[0] = "TAMPERED"
	d2 := Diagnose(badChecksumGGA)
	assert.Equal(t, "203415.000", d2.Fields[0])

	var cErr *ChecksumError
	require.True(t, errors.As(d2.Err, &cErr))
	cErr.Sentence.Fields[0] = "TAMPERED"
	d3 := Diagnose(badChecksumGGA)
	assert.Equal(t, "203415.000", d3.Fields[0])

	// Diagnostic type has no Sentence implementation: a type assertion fails.
	var _ interface{} = d
	if _, ok := interface{}(d).(Sentence); ok {
		t.Fatal("Diagnostic must not implement Sentence, no trusted object can leak")
	}
}

// TestDefaultParseRegression verifies the default Parse interface behaves
// exactly as before when the new option is not enabled: plain error messages,
// no typed wrapping, valid sentences still produce typed objects.
func TestDefaultParseRegression(t *testing.T) {
	t.Run("valid GGA still parses to a trusted positioning object", func(t *testing.T) {
		s, err := Parse(goodGGA)
		require.NoError(t, err)
		gga, ok := s.(GGA)
		require.True(t, ok)
		assert.Equal(t, "1", gga.FixQuality)
		assert.InDelta(t, 63.4268967, gga.Latitude, 1e-6)
	})

	t.Run("bad checksum: default Parse returns the original plain error", func(t *testing.T) {
		s, err := Parse(badChecksumGGA)
		assert.Nil(t, s, "no trusted object on checksum failure")
		require.Error(t, err)
		assert.Equal(t, "nmea: sentence checksum mismatch [7C != 00]", err.Error())
		// Must not be wrapped when the option is off.
		assert.False(t, errors.Is(err, ErrChecksum))
		assert.False(t, errors.Is(err, ErrFormat))
		var cErr *ChecksumError
		assert.False(t, errors.As(err, &cErr))
	})

	t.Run("malformed header: default Parse returns the original plain error", func(t *testing.T) {
		s, err := Parse(malformedGGA)
		assert.Nil(t, s)
		require.Error(t, err)
		assert.Equal(t, "nmea: sentence does not start with a '$' or '!'", err.Error())
		var fErr *FormatError
		assert.False(t, errors.As(err, &fErr))
	})

	t.Run("zero-value SentenceParser keeps the old interface usable", func(t *testing.T) {
		p := &SentenceParser{}
		s, err := p.Parse(goodGGA)
		require.NoError(t, err)
		assert.Equal(t, "GGA", s.DataType())

		_, err = p.Parse(badChecksumGGA)
		require.Error(t, err)
		assert.Equal(t, "nmea: sentence checksum mismatch [7C != 00]", err.Error())
		assert.False(t, errors.Is(err, ErrChecksum))
	})
}

// TestParseWithDiagnoseChecksumFailure verifies the opt-in flag on Parse:
// checksum failures bypass object construction with a typed *ChecksumError
// carrying the original sentence parts, while format errors are not bypassed
// and valid sentences still parse normally.
func TestParseWithDiagnoseChecksumFailure(t *testing.T) {
	p := &SentenceParser{DiagnoseChecksumFailure: true}

	t.Run("valid sentence still yields trusted object when flag is on", func(t *testing.T) {
		s, err := p.Parse(goodGGA)
		require.NoError(t, err)
		gga, ok := s.(GGA)
		require.True(t, ok)
		assert.Equal(t, 72.5, gga.Altitude)
	})

	t.Run("bad checksum bypasses with typed error and original fields", func(t *testing.T) {
		s, err := p.Parse(badChecksumGGA)
		assert.Nil(t, s, "must not produce a trusted positioning object")
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrChecksum))
		assert.False(t, errors.Is(err, ErrFormat))

		var cErr *ChecksumError
		require.True(t, errors.As(err, &cErr))
		assert.Equal(t, "7C", cErr.Expected)
		assert.Equal(t, "00", cErr.Actual)
		assert.Equal(t, "GN", cErr.Sentence.Talker)
		assert.Equal(t, "GGA", cErr.Sentence.Type)
		assert.Equal(t, expectedRawFields, cErr.Sentence.Fields)
		// error chain retains the original message
		assert.Contains(t, err.Error(), "nmea: sentence checksum mismatch [7C != 00]")
	})

	t.Run("malformed header is still a plain format failure, not bypassed", func(t *testing.T) {
		s, err := p.Parse(malformedGGA)
		assert.Nil(t, s)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrChecksum))
		assert.Equal(t, "nmea: sentence does not start with a '$' or '!'", err.Error())
	})

	t.Run("missing checksum separator is not bypassed", func(t *testing.T) {
		s, err := p.Parse("$GPFOO,1,2,3,x,y,z")
		assert.Nil(t, s)
		require.Error(t, err)
		assert.False(t, errors.Is(err, ErrChecksum))
		assert.Contains(t, err.Error(), "does not contain checksum separator")
	})
}

// TestAcceptanceReport is the deterministic acceptance fixture: it runs the
// three samples plus the default-interface regression and writes a small,
// human readable report into the test temporary directory (t.TempDir is
// removed by the test framework) and prints the path so it can be inspected.
func TestAcceptanceReport(t *testing.T) {
	if testing.Short() {
		t.Skip("acceptance report skipped in -short mode")
	}

	samples := []struct {
		name string
		raw  string
	}{
		{"valid", goodGGA},
		{"bad-checksum", badChecksumGGA},
		{"malformed-header", malformedGGA},
	}

	var b strings.Builder
	b.WriteString("=== NMEA checksum-bypass diagnostic acceptance ===\n\n")
	b.WriteString("-- Diagnose (read-only entry) --\n")
	for _, sm := range samples {
		d := Diagnose(sm.raw)
		fmt.Fprintf(&b, "sample: %s\n", sm.name)
		fmt.Fprintf(&b, "  kind    : %s\n", d.Kind)
		fmt.Fprintf(&b, "  talker  : %q type: %q prefix: %q\n", d.Talker, d.Type, d.Prefix())
		fmt.Fprintf(&b, "  checksum: %q\n", d.Checksum)
		fmt.Fprintf(&b, "  fields  : %q\n", d.Fields)
		fmt.Fprintf(&b, "  trusted : %v (must be false)\n", d.TrustedSentence())
		if d.Err != nil {
			fmt.Fprintf(&b, "  err     : %v\n", d.Err)
			fmt.Fprintf(&b, "  isChecksumErr: %v  isFormatErr: %v\n",
				errors.Is(d.Err, ErrChecksum), errors.Is(d.Err, ErrFormat))
			var cErr *ChecksumError
			if errors.As(d.Err, &cErr) {
				fmt.Fprintf(&b, "  chain   : %v (computed=%s received=%s)\n",
					cErr.Unwrap(), cErr.Expected, cErr.Actual)
			} else {
				var fErr *FormatError
				if errors.As(d.Err, &fErr) {
					fmt.Fprintf(&b, "  chain   : %v\n", fErr.Unwrap())
				}
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("-- Default Parse regression (option disabled) --\n")
	for _, sm := range samples {
		s, err := Parse(sm.raw)
		obj := "<none>"
		if s != nil {
			obj = fmt.Sprintf("%T (trusted positioning object)", s)
		}
		fmt.Fprintf(&b, "sample: %-16s object: %-45s err: %v\n", sm.name, obj, err)
	}
	b.WriteString("\n-- Opt-in Parse{DiagnoseChecksumFailure:true} --\n")
	p := &SentenceParser{DiagnoseChecksumFailure: true}
	for _, sm := range samples {
		s, err := p.Parse(sm.raw)
		obj := "<none>"
		if s != nil {
			obj = fmt.Sprintf("%T", s)
		}
		fmt.Fprintf(&b, "sample: %-16s object: %-10s err: %v\n", sm.name, obj, err)
	}

	report := b.String()
	dir := t.TempDir()
	path := filepath.Join(dir, "diagnostic-acceptance.txt")
	require.NoError(t, os.WriteFile(path, []byte(report), 0o600))
	t.Logf("acceptance report written to: %s\n%s", path, report)

	// Basic sanity assertions on the report content.
	assert.Contains(t, report, "kind    : ok")
	assert.Contains(t, report, "kind    : checksum-error")
	assert.Contains(t, report, "kind    : format-error")
	assert.Contains(t, report, "trusted : false")
	assert.Contains(t, report, "nmea: sentence checksum mismatch [7C != 00]")
	assert.Contains(t, report, "nmea.GGA (trusted positioning object)")
}
