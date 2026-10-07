package nmea

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sampleValid = "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A"
	// same sentence as sampleValid but with a corrupted checksum
	sampleBadCRC = "$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*00"
	// malformed header: missing the leading '$'
	sampleBadHeader = "GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A"
)

var wantRMCFields = []string{"123519", "A", "4807.038", "N", "01131.000", "E", "022.4", "084.4", "230394", "003.1", "W"}

func checkRMCDiagnostic(t *testing.T, diag SentenceDiagnostic) {
	t.Helper()
	if diag.Talker != "GP" {
		t.Errorf("expected talker GP, got %q", diag.Talker)
	}
	if diag.Type != "RMC" {
		t.Errorf("expected type RMC, got %q", diag.Type)
	}
	if diag.Prefix() != "GPRMC" {
		t.Errorf("expected prefix GPRMC, got %q", diag.Prefix())
	}
	if len(diag.Fields) != len(wantRMCFields) {
		t.Fatalf("expected %d fields, got %d (%v)", len(wantRMCFields), len(diag.Fields), diag.Fields)
	}
	for i, want := range wantRMCFields {
		if diag.Fields[i] != want {
			t.Errorf("field %d: expected %q, got %q", i, want, diag.Fields[i])
		}
	}
}

func TestDiagnoseValidSentence(t *testing.T) {
	diag, err := Diagnose(sampleValid)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	checkRMCDiagnostic(t, diag)
	if diag.Checksum != "6A" {
		t.Errorf("expected checksum 6A, got %q", diag.Checksum)
	}
	if diag.Raw != sampleValid {
		t.Errorf("expected raw %q, got %q", sampleValid, diag.Raw)
	}

	// the default interface keeps producing a trusted positioning object
	s, err := Parse(sampleValid)
	if err != nil {
		t.Fatalf("Parse: expected no error, got %v", err)
	}
	if _, ok := s.(RMC); !ok {
		t.Errorf("Parse: expected RMC sentence, got %T", s)
	}
}

func TestDiagnoseChecksumMismatch(t *testing.T) {
	diag, err := Diagnose(sampleBadCRC)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	// error is classified as a checksum error, not a format error
	var diagErr *DiagnosticError
	if !errors.As(err, &diagErr) {
		t.Fatalf("expected *DiagnosticError, got %T", err)
	}
	if diagErr.Kind != DiagnosticChecksumError {
		t.Errorf("expected kind %v, got %v", DiagnosticChecksumError, diagErr.Kind)
	}

	// the original error chain is preserved: DiagnosticError -> ChecksumError -> checksum mismatch
	var csErr *ChecksumError
	if !errors.As(err, &csErr) {
		t.Fatalf("expected *ChecksumError in chain, got %T", err)
	}
	if !strings.Contains(errors.Unwrap(errors.Unwrap(err)).Error(), "checksum mismatch") {
		t.Errorf("expected root cause to mention checksum mismatch, got %v", errors.Unwrap(errors.Unwrap(err)))
	}

	// the original talker, type and raw fields are still available
	checkRMCDiagnostic(t, diag)
	if diag.Checksum != "00" {
		t.Errorf("expected raw checksum 00, got %q", diag.Checksum)
	}
	if csErr.Sentence.Prefix() != "GPRMC" {
		t.Errorf("expected ChecksumError sentence prefix GPRMC, got %q", csErr.Sentence.Prefix())
	}

	// no trusted positioning object can be obtained through the default interface
	s, perr := Parse(sampleBadCRC)
	if perr == nil {
		t.Error("Parse: expected an error for corrupted checksum, got nil")
	}
	if s != nil {
		t.Errorf("Parse: expected no trusted sentence, got %v", s)
	}
}

func TestDiagnoseMalformedHeader(t *testing.T) {
	diag, err := Diagnose(sampleBadHeader)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}

	// error is classified as a format error, not a checksum error
	var diagErr *DiagnosticError
	if !errors.As(err, &diagErr) {
		t.Fatalf("expected *DiagnosticError, got %T", err)
	}
	if diagErr.Kind != DiagnosticFormatError {
		t.Errorf("expected kind %v, got %v", DiagnosticFormatError, diagErr.Kind)
	}
	var csErr *ChecksumError
	if errors.As(err, &csErr) {
		t.Error("did not expect *ChecksumError in chain for a format error")
	}
	if !strings.Contains(err.Error(), "does not start with") {
		t.Errorf("expected original format error to be preserved, got %v", err)
	}

	// header could not be decoded, so no talker/type/fields are available
	if diag.Talker != "" || diag.Type != "" || len(diag.Fields) != 0 {
		t.Errorf("expected empty diagnostic, got %+v", diag)
	}

	// no trusted positioning object can be obtained through the default interface
	s, perr := Parse(sampleBadHeader)
	if perr == nil {
		t.Error("Parse: expected an error for malformed header, got nil")
	}
	if s != nil {
		t.Errorf("Parse: expected no trusted sentence, got %v", s)
	}
}

// TestDiagnoseFixtureFile demonstrates the diagnostic entry point against a
// small fixture file held in a temporary directory.
func TestDiagnoseFixtureFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "samples.txt")
	content := strings.Join([]string{sampleValid, sampleBadCRC, sampleBadHeader}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write fixture: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	wantKinds := []string{"none", "checksum", "format"}
	for i, line := range strings.Split(string(data), "\n") {
		diag, derr := Diagnose(line)
		kind := "none"
		var dErr *DiagnosticError
		if errors.As(derr, &dErr) {
			kind = dErr.Kind.String()
		}
		t.Logf("sample[%d] talker=%q type=%q fields=%d checksum=%q kind=%s err=%v",
			i, diag.Talker, diag.Type, len(diag.Fields), diag.Checksum, kind, derr)
		if kind != wantKinds[i] {
			t.Errorf("sample[%d]: expected kind %q, got %q", i, wantKinds[i], kind)
		}
	}
}
