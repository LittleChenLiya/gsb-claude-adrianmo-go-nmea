package nmea

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// Fixed sentences used across the aggregator tests. Checksums are fixed.
const (
	// six-sentence timing example (two talkers, one valid and one invalid GP epoch)
	fixGGAValid = "$GPGGA,120001.00,5955.1234,N,01043.5678,E,1,08,1.2,10.0,M,30.0,M,,*55"
	fixRMCA     = "$GPRMC,120001.00,A,5955.1234,N,01043.5678,E,10.5,45.0,061026,,,A*58"
	fixGGAInv   = "$GPGGA,120002.00,5955.0000,N,01043.0000,E,0,04,9.9,10.0,M,30.0,M,,*50"
	fixRMCV     = "$GPRMC,120002.00,V,,,,,,,061026,,,N*7F"
	fixGNGGA    = "$GNGGA,120001.00,5000.0000,N,00800.0000,E,1,05,1.5,50.0,M,40.0,M,,*4D"
	fixGNRMC    = "$GNRMC,120001.00,A,5000.0000,N,00800.0000,E,0.0,0.0,061026,,,A*4C"

	// midnight ambiguity: a GGA near the day boundary without a same-instant RMC
	fixMidGGA   = "$GPGGA,235958.00,5900.0000,N,01000.0000,E,1,08,1.0,5.0,M,20.0,M,,*6F"
	fixMidRMCD1 = "$GPRMC,235959.00,A,5900.0001,N,01000.0001,E,0.0,0.0,051026,,,A*52"
	fixMidRMCD2 = "$GPRMC,000002.00,A,5900.0002,N,01000.0002,E,0.0,0.0,061026,,,A*52"

	// finite late window
	fixLateGGA1 = "$GPGGA,120000.00,4000.0000,N,00200.0000,E,1,08,1.0,5.0,M,20.0,M,,*67"
	fixLateGGA2 = "$GPGGA,120130.00,4001.0000,N,00201.0000,E,1,08,1.0,5.0,M,20.0,M,,*65"
	fixLateRMC2 = "$GPRMC,120130.00,A,4001.0000,N,00201.0000,E,0.0,0.0,061026,,,A*5A"
	fixLateRMC1 = "$GPRMC,120000.00,A,4000.0000,N,00200.0000,E,0.0,0.0,061026,,,A*58"

	// conflicting coordinates for the same instant
	fixConflictGGA = "$GPGGA,120000.00,1000.0000,N,00100.0000,E,1,08,1.0,5.0,M,20.0,M,,*61"
	fixConflictRMC = "$GPRMC,120000.00,A,1000.0100,N,00100.0100,E,0.0,0.0,061026,,,A*5E"
)

func sourceTypes(f PositionFix) []string {
	types := make([]string, 0, len(f.Sources))
	for _, s := range f.Sources {
		types = append(types, s.Type)
	}
	return types
}

// feedAll feeds the raw sentences in the given order and returns everything
// the aggregator emits, including the final Flush.
func feedAll(t *testing.T, raws ...string) []PositionFix {
	t.Helper()
	a := NewPositionAggregator()
	a.ReferenceYear = 2026
	var fixes []PositionFix
	for _, raw := range raws {
		got, err := a.Add(raw)
		assert.NoError(t, err)
		fixes = append(fixes, got...)
	}
	fixes = append(fixes, a.Flush()...)
	return fixes
}

func TestPositionAggregatorOrderIndependent(t *testing.T) {
	// GGA first, then RMC.
	fwd := feedAll(t, fixGGAValid, fixRMCA)
	if assert.Len(t, fwd, 1) {
		f := fwd[0]
		assert.True(t, f.Complete)
		assert.True(t, f.Valid)
		assert.False(t, f.Uncertain)
		assert.False(t, f.Stale)
		assert.True(t, f.DateKnown)
		assert.Equal(t, "GP", f.Talker)
		assert.Equal(t, []string{TypeGGA, TypeRMC}, sourceTypes(f))
		assert.Equal(t, time.Date(2026, time.October, 6, 12, 0, 1, 0, time.UTC), f.Time)
		// sources preserve the originating sentence type and raw sentence
		assert.Equal(t, TypeGGA, f.Sources[0].Type)
		assert.Equal(t, fixGGAValid, f.Sources[0].Raw)
		assert.Equal(t, TypeRMC, f.Sources[1].Type)
		assert.Equal(t, fixRMCA, f.Sources[1].Raw)
		assert.InDelta(t, MustParseLatLong("5955.1234 N"), f.Latitude, 1e-9)
		assert.InDelta(t, 10.5, f.Speed, 1e-9)
		assert.InDelta(t, 45.0, f.Course, 1e-9)
		assert.InDelta(t, 10.0, f.Altitude, 1e-9)
	}

	// RMC first, then GGA (reverse order, same instant): the combined fix
	// must be identical and not depend on which sentence arrived first.
	rev := feedAll(t, fixRMCA, fixGGAValid)
	if assert.Len(t, rev, 1) {
		assert.Equal(t, fwd[0], rev[0])
	}
}

func TestPositionAggregatorInvalidRMCDoesNotValidateGGA(t *testing.T) {
	// GGA carries fix quality 0 and RMC is invalid ("V"): the combined fix
	// is not complete/valid.
	fixes := feedAll(t, fixGGAInv, fixRMCV)
	if assert.Len(t, fixes, 1) {
		f := fixes[0]
		assert.False(t, f.Complete, "invalid RMC must not yield a complete fix")
		assert.False(t, f.Valid, "invalid RMC must not make the GGA fix valid")
		assert.True(t, f.DateKnown)
		assert.NotNil(t, f.GGA)
		assert.NotNil(t, f.RMC)
		assert.Equal(t, InvalidRMC, f.RMC.Validity)
	}

	// A valid GGA paired with an invalid RMC for the same instant must still
	// not be reported as a valid full fix.
	fixes = feedAll(t, fixGGAValid,
		"$GPRMC,120001.00,V,5955.1234,N,01043.5678,E,10.5,45.0,061026,,,N*40")
	if assert.Len(t, fixes, 1) {
		assert.False(t, fixes[0].Complete)
		assert.False(t, fixes[0].Valid)
	}
}

func TestPositionAggregatorMidnightAmbiguity(t *testing.T) {
	// The 235958 GGA has no RMC for the same time of day. It must not be
	// assigned the date of the neighbouring 235959 RMC (051026) or of the
	// 000002 RMC (061026).
	fixes := feedAll(t, fixMidGGA, fixMidRMCD1, fixMidRMCD2)

	var ggaFix, rmcD1, rmcD2 *PositionFix
	for i := range fixes {
		f := &fixes[i]
		switch {
		case f.GGA != nil && f.RMC == nil:
			ggaFix = f
		case f.RMC != nil && f.RMC.Time.Hour == 23:
			rmcD1 = f
		case f.RMC != nil && f.RMC.Time.Hour == 0:
			rmcD2 = f
		}
	}
	if assert.NotNil(t, ggaFix) {
		assert.False(t, ggaFix.DateKnown, "GGA without a same-instant RMC must keep an unknown date")
		assert.False(t, ggaFix.Complete)
		assert.False(t, ggaFix.Valid)
		assert.Equal(t, []string{TypeGGA}, sourceTypes(*ggaFix))
		assert.True(t, ggaFix.Time.IsZero())
	}
	if assert.NotNil(t, rmcD1) {
		assert.True(t, rmcD1.DateKnown)
		assert.Equal(t, time.Date(2026, time.October, 5, 23, 59, 59, 0, time.UTC), rmcD1.Time)
	}
	if assert.NotNil(t, rmcD2) {
		assert.True(t, rmcD2.DateKnown)
		assert.Equal(t, time.Date(2026, time.October, 6, 0, 0, 2, 0, time.UTC), rmcD2.Time)
	}
}

func TestPositionAggregatorLateWindow(t *testing.T) {
	a := NewPositionAggregator()
	a.ReferenceYear = 2026

	// First GGA (120000) waits for its RMC.
	got, err := a.Add(fixLateGGA1)
	assert.NoError(t, err)
	assert.Empty(t, got)

	// Second GGA (120130): no dated RMC has anchored the stream clock yet,
	// so nothing can be expired.
	got, err = a.Add(fixLateGGA2)
	assert.NoError(t, err)
	assert.Empty(t, got)

	// RMC for 120130 completes the second fix and anchors the clock.
	got, err = a.Add(fixLateRMC2)
	assert.NoError(t, err)
	if assert.Len(t, got, 1) {
		assert.True(t, got[0].Complete)
		assert.True(t, got[0].Valid)
	}

	// RMC for 120000 arrives 90s of stream time later, beyond the default
	// 30s window. It is emitted immediately as a stale, RMC-only partial
	// fix and is never glued to the waiting GGA by arrival order.
	got, err = a.Add(fixLateRMC1)
	assert.NoError(t, err)
	if assert.Len(t, got, 1) {
		f := got[0]
		assert.True(t, f.Stale)
		assert.False(t, f.Complete)
		assert.True(t, f.DateKnown)
		assert.Nil(t, f.GGA)
		assert.NotNil(t, f.RMC)
		assert.Equal(t, []string{TypeRMC}, sourceTypes(f))
	}

	// Flushing the still-waiting 120000 GGA keeps the date unknown: the late
	// RMC must not retroactively supply a guessed date.
	rest := a.Flush()
	if assert.Len(t, rest, 1) {
		assert.False(t, rest[0].DateKnown)
		assert.False(t, rest[0].Complete)
		assert.NotNil(t, rest[0].GGA)
		assert.Nil(t, rest[0].RMC)
	}
}

func TestPositionAggregatorConflictingCoordinates(t *testing.T) {
	// The two sentences report coordinates roughly 26m apart for the same
	// talker and UTC time of day: the fix is uncertain and not complete.
	fixes := feedAll(t, fixConflictGGA, fixConflictRMC)
	if assert.Len(t, fixes, 1) {
		f := fixes[0]
		assert.True(t, f.Uncertain)
		assert.False(t, f.Complete)
		assert.False(t, f.Valid)
		assert.True(t, f.DateKnown)
		// GGA coordinates remain the chosen coordinates of the fix.
		assert.InDelta(t, MustParseLatLong("1000.0000 N"), f.Latitude, 1e-9)
	}

	// With a tolerance larger than the disagreement, the same pair is a
	// complete, valid fix.
	a := NewPositionAggregator()
	a.ReferenceYear = 2026
	a.CoordTolerance = 100
	got, err := a.Add(fixConflictGGA)
	assert.NoError(t, err)
	assert.Empty(t, got)
	got, err = a.Add(fixConflictRMC)
	assert.NoError(t, err)
	if assert.Len(t, got, 1) {
		assert.True(t, got[0].Complete)
		assert.True(t, got[0].Valid)
		assert.False(t, got[0].Uncertain)
	}
}

func TestPositionAggregatorDifferentTalkersNotCombined(t *testing.T) {
	// Same time of day but different talkers must never be paired.
	fixes := feedAll(t, fixGGAValid, fixGNRMC)
	if assert.Len(t, fixes, 2) {
		for _, f := range fixes {
			assert.False(t, f.Complete)
			assert.Len(t, f.Sources, 1)
		}
		assert.Equal(t, "GN", fixes[0].Talker) // Flush orders talkers lexically
		assert.Equal(t, "GP", fixes[1].Talker)
	}
}

func TestPositionAggregatorIgnoresOtherTypes(t *testing.T) {
	a := NewPositionAggregator()
	assert.Nil(t, a.AddSentence(nil))
	// VTG is supported by the parser but must be ignored by the aggregator.
	other, err := Parse("$GPVTG,45.0,T,,M,10.0,N,18.5,K,A*31")
	assert.NoError(t, err)
	assert.Nil(t, a.AddSentence(other))
	assert.Empty(t, a.Flush())
}

// ExamplePositionAggregator feeds the six-sentence timing example: a valid and
// an invalid GP epoch plus a valid GN epoch. GGA and RMC for the same talker
// and UTC time of day are combined regardless of their arrival order, and the
// invalid RMC epoch is never a valid full fix.
func ExamplePositionAggregator() {
	a := NewPositionAggregator()
	a.ReferenceYear = 2026
	feed := func(raw string) {
		fixes, err := a.Add(raw)
		if err != nil {
			fmt.Println("error:", err)
			return
		}
		for _, f := range fixes {
			fmt.Printf("%s %s complete=%t valid=%t sources=%v\n",
				f.Talker, f.Time.Format("2006-01-02T15:04:05Z"),
				f.Complete, f.Valid, sourceTypes(f))
		}
	}
	feed(fixGGAValid)
	feed(fixRMCA)
	feed(fixGGAInv)
	feed(fixRMCV)
	feed(fixGNGGA)
	feed(fixGNRMC)
	// Output:
	// GP 2026-10-06T12:00:01Z complete=true valid=true sources=[GGA RMC]
	// GP 2026-10-06T12:00:02Z complete=false valid=false sources=[GGA RMC]
	// GN 2026-10-06T12:00:01Z complete=true valid=true sources=[GGA RMC]
}
