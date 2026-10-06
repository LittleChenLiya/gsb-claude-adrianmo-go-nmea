package nmea

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed sentences used by the aggregator tests. All carry valid checksums.
const (
	gnRMC101530Valid = "$GNRMC,101530.000,A,3356.4650,S,15124.5567,E,0.0,0.0,130694,0.0,E,A*0F"
	gnGGA101530Valid = "$GNGGA,101530.000,3356.4650,S,15124.5567,E,1,08,9.7,25.0,M,21.0,M,,*6D"
	gnGGA101531Valid = "$GNGGA,101531.000,3356.4650,S,15124.5567,E,1,08,9.7,25.0,M,21.0,M,,*6C"
	gnRMC101531Inv   = "$GNRMC,101531.000,V,3356.4650,S,15124.5567,E,0.0,0.0,130694,0.0,E,A*19"
	gnGGA235958Valid = "$GNGGA,235958.000,3356.4650,S,15124.5567,E,1,08,9.7,25.0,M,21.0,M,,*6B"
	gnRMC000002Valid = "$GNRMC,000002.000,A,3356.4650,S,15124.5567,E,0.0,0.0,140694,0.0,E,A*0C"

	gpRMC101530Valid = "$GPRMC,101530.000,A,3356.4650,S,15124.5567,E,0.0,0.0,130694,0.0,E,A*11"
	gpGGA101530Valid = "$GPGGA,101530.000,3356.4650,S,15124.5567,E,1,08,9.7,25.0,M,21.0,M,,*73"

	gnGGA101530Invalid = "$GNGGA,101530.000,3356.4650,S,15124.5567,E,0,08,9.7,25.0,M,21.0,M,,*6C"
	gnRMC101530ValidB  = "$GNRMC,101530.000,A,3356.4650,S,15124.5567,E,0.0,0.0,130694,0.0,E,A*0F"

	gnRMC101530Conflict = "$GNRMC,101530.000,A,3357.4650,S,15124.5567,E,0.0,0.0,130694,0.0,E,A*0E"
)

func parseSentence(t *testing.T, raw string) Sentence {
	t.Helper()
	s, err := Parse(raw)
	require.NoError(t, err)
	return s
}

// TestPositionAggregator_SixSentenceTiming is the six-sentence timing example.
// It exercises, in order:
//  1. RMC arrives before its GGA (reverse arrival) -> no fix yet
//  2. GGA arrives, pairs with the RMC -> complete valid fix
//  3. a new GGA epoch with no RMC yet -> no fix
//  4. the matching RMC is invalid -> fix formed but invalid (status preserved)
//  5. a dateless GGA just before midnight -> no fix
//  6. an RMC just after midnight on the next date -> wall gap > window, no date
//     is guessed across midnight, no fix
func TestPositionAggregator_SixSentenceTiming(t *testing.T) {
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	agg.Window = 500 * time.Millisecond

	// 1. RMC 10:15:30 valid, arrives first.
	fix := agg.Update(parseSentence(t, gnRMC101530Valid))
	assert.Nil(t, fix, "RMC without a GGA must not produce a fix")

	// 2. GGA 10:15:30 valid, pairs with the RMC.
	fix = agg.Update(parseSentence(t, gnGGA101530Valid))
	require.NotNil(t, fix)
	assert.True(t, fix.Valid, "expected a complete valid fix")
	assert.False(t, fix.Uncertain)
	assert.Equal(t, "GN", fix.Talker)
	assert.Equal(t, time.Date(1994, 6, 13, 10, 15, 30, 0, time.UTC), fix.Time)
	assert.InDelta(t, MustParseLatLong("3356.4650 S"), fix.Latitude, 1e-9)
	assert.InDelta(t, MustParseLatLong("15124.5567 E"), fix.Longitude, 1e-9)
	assert.Equal(t, GPS, fix.FixQuality)
	assert.Equal(t, TypeRMC, fix.DateSource, "date must be sourced from the RMC")
	assert.Equal(t, []string{TypeGGA, TypeRMC}, fix.Sources, "original sentence types must be preserved")

	// 3. GGA 10:15:31 valid, no RMC at that epoch yet.
	fix = agg.Update(parseSentence(t, gnGGA101531Valid))
	assert.Nil(t, fix, "GGA without a same-epoch RMC must not produce a fix")

	// 4. RMC 10:15:31 invalid (V), pairs with the GGA but the fix is invalid.
	fix = agg.Update(parseSentence(t, gnRMC101531Inv))
	require.NotNil(t, fix)
	assert.False(t, fix.Valid, "an invalid RMC must invalidate the fix")
	assert.False(t, fix.Uncertain)
	assert.Equal(t, GPS, fix.FixQuality, "GGA fix quality is preserved, not promoted")
	assert.Equal(t, time.Date(1994, 6, 13, 10, 15, 31, 0, time.UTC), fix.Time)
	assert.Equal(t, TypeRMC, fix.DateSource)
	assert.Equal(t, []string{TypeGGA, TypeRMC}, fix.Sources)

	// 5. Dateless GGA 23:59:58 with no RMC near it.
	fix = agg.Update(parseSentence(t, gnGGA235958Valid))
	assert.Nil(t, fix, "dateless GGA without a same-epoch RMC must not produce a fix")

	// 6. RMC 00:00:02 on the next date. Wall gap to the 23:59:58 GGA is ~4h... no,
	//    it is ~24h in wall-clock terms (23:59:58 -> 00:00:02), which exceeds the
	//    window. The date is not guessed across midnight.
	fix = agg.Update(parseSentence(t, gnRMC000002Valid))
	assert.Nil(t, fix, "cross-midnight pair must not guess the date and must not produce a fix")
}

// TestPositionAggregator_ReverseOrderSameResult verifies that feeding the same
// GGA/RMC pair in opposite arrival orders yields the identical fix. Pairing is
// driven by the real UTC instant, not by arrival order.
func TestPositionAggregator_ReverseOrderSameResult(t *testing.T) {
	gga := parseSentence(t, gpGGA101530Valid)
	rmc := parseSentence(t, gpRMC101530Valid)

	aggA := NewPositionAggregator()
	aggA.ReferenceYear = 1994
	fixA1 := aggA.Update(gga)
	fixA2 := aggA.Update(rmc)

	aggB := NewPositionAggregator()
	aggB.ReferenceYear = 1994
	fixB1 := aggB.Update(rmc)
	fixB2 := aggB.Update(gga)

	assert.Nil(t, fixA1)
	assert.Nil(t, fixB1)
	require.NotNil(t, fixA2)
	require.NotNil(t, fixB2)
	assert.Equal(t, fixA2, fixB2, "reverse arrival order must produce the identical fix")
	assert.True(t, fixA2.Valid)
	assert.Equal(t, time.Date(1994, 6, 13, 10, 15, 30, 0, time.UTC), fixA2.Time)
}

// TestPositionAggregator_RMCInvalidDoesNotMakeGGAValid verifies that an invalid
// GGA (fix quality 0) is never promoted by a valid RMC, and that an invalid
// RMC invalidates an otherwise valid GGA. Invalid status is preserved.
func TestPositionAggregator_RMCInvalidDoesNotMakeGGAValid(t *testing.T) {
	// GGA invalid (quality 0) + RMC valid -> still invalid.
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	agg.Update(parseSentence(t, gnGGA101530Invalid))
	fix := agg.Update(parseSentence(t, gnRMC101530ValidB))
	require.NotNil(t, fix)
	assert.False(t, fix.Valid, "a valid RMC must not promote an invalid GGA")
	assert.Equal(t, Invalid, fix.FixQuality, "GGA fix quality (invalid) is preserved")
	assert.Equal(t, TypeRMC, fix.DateSource)

	// GGA valid + RMC invalid -> invalid (covered in the timing example, but
	// asserted here explicitly for symmetry).
	agg2 := NewPositionAggregator()
	agg2.ReferenceYear = 1994
	agg2.Update(parseSentence(t, gnGGA101531Valid))
	fix2 := agg2.Update(parseSentence(t, gnRMC101531Inv))
	require.NotNil(t, fix2)
	assert.False(t, fix2.Valid, "an invalid RMC must invalidate a valid GGA")
}

// TestPositionAggregator_ConflictingCoordinatesUncertain verifies that when the
// GGA and RMC report coordinates that disagree beyond the tolerance, the fix is
// marked uncertain and is never complete.
func TestPositionAggregator_ConflictingCoordinatesUncertain(t *testing.T) {
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	agg.CoordTolerance = 1e-5
	agg.Update(parseSentence(t, gnGGA101530Valid))
	fix := agg.Update(parseSentence(t, gnRMC101530Conflict))
	require.NotNil(t, fix)
	assert.True(t, fix.Uncertain, "conflicting coordinates must mark the fix uncertain")
	assert.False(t, fix.Valid, "an uncertain fix is never complete")
}

// TestPositionAggregator_SourcePreservation verifies that the fix records the
// original sentence types it was combined from and where the date came from.
func TestPositionAggregator_SourcePreservation(t *testing.T) {
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	agg.Update(parseSentence(t, gnRMC101530Valid))
	fix := agg.Update(parseSentence(t, gnGGA101530Valid))
	require.NotNil(t, fix)
	assert.Equal(t, []string{TypeGGA, TypeRMC}, fix.Sources)
	assert.Equal(t, TypeRMC, fix.DateSource)
}

// TestPositionAggregator_LateSentenceFiniteWindow verifies that a sentence
// arriving late (out of arrival order) is still matched within the finite
// window, but once the hold window expires the unmatched sentence is discarded
// and no fix is produced.
func TestPositionAggregator_LateSentenceFiniteWindow(t *testing.T) {
	// Within the window: RMC arrives first, GGA arrives late but same epoch.
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	agg.MaxHold = 200 * time.Millisecond
	agg.Update(parseSentence(t, gnRMC101530Valid))
	time.Sleep(50 * time.Millisecond)
	fix := agg.Update(parseSentence(t, gnGGA101530Valid))
	require.NotNil(t, fix, "a late GGA within the hold window must still pair")
	assert.True(t, fix.Valid)

	// Beyond the window: the unmatched RMC is expired before the GGA arrives.
	agg2 := NewPositionAggregator()
	agg2.ReferenceYear = 1994
	agg2.MaxHold = 50 * time.Millisecond
	agg2.Update(parseSentence(t, gnRMC101530Valid))
	time.Sleep(80 * time.Millisecond)
	fix2 := agg2.Update(parseSentence(t, gnGGA101530Valid))
	assert.Nil(t, fix2, "a sentence arriving after the hold window must not pair")
}

// TestPositionAggregator_SeparatesTalkers verifies that GGA and RMC from
// different talkers are not combined.
func TestPositionAggregator_SeparatesTalkers(t *testing.T) {
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	// GN RMC arrives, then GP GGA: different talkers, must not pair.
	agg.Update(parseSentence(t, gnRMC101530Valid))
	fix := agg.Update(parseSentence(t, gpGGA101530Valid))
	assert.Nil(t, fix, "sentences from different talkers must not be combined")

	// Same-talker pair still works.
	fix = agg.Update(parseSentence(t, gpRMC101530Valid))
	require.NotNil(t, fix)
	assert.Equal(t, "GP", fix.Talker)
	assert.True(t, fix.Valid)
}

// TestPositionAggregator_IgnoresOtherTypes verifies that non-GGA/RMC sentences
// are ignored and do not affect pairing.
func TestPositionAggregator_IgnoresOtherTypes(t *testing.T) {
	agg := NewPositionAggregator()
	agg.ReferenceYear = 1994
	agg.Update(parseSentence(t, "$GNGSA,A,3,01,02,03,04,05,06,07,08,09,10,11,12,1.0,1.0,1.0*2E"))
	agg.Update(parseSentence(t, gnRMC101530Valid))
	fix := agg.Update(parseSentence(t, gnGGA101530Valid))
	require.NotNil(t, fix)
	assert.True(t, fix.Valid)
}
