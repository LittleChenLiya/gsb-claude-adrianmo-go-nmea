package nmea

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// PositionFix is a time-referenced position produced by aggregating a GGA and
// an RMC sentence from the same talker.
//
// A fix is "complete" only when Valid is true: both sentences report a valid
// fix, the real UTC instant can be determined without guessing the date, and
// the GGA and RMC coordinates agree. Invalid fix status is preserved, so a
// valid RMC can never promote an invalid GGA.
type PositionFix struct {
	// Talker is the talker id shared by the paired GGA and RMC (e.g. "GN").
	Talker string
	// Time is the real UTC instant of the fix. It is zero when the date could
	// not be resolved (cross-midnight ambiguity).
	Time time.Time
	// Latitude in decimal degrees, sourced from the GGA.
	Latitude float64
	// Longitude in decimal degrees, sourced from the GGA.
	Longitude float64
	// Valid reports whether the fix is complete and certain: GGA fix quality is
	// not Invalid, RMC validity is ValidRMC, the instant is known, and the
	// coordinates agree.
	Valid bool
	// Uncertain reports that the GGA and RMC coordinates disagree beyond the
	// configured tolerance. An uncertain fix is never complete.
	Uncertain bool
	// FixQuality is the fix quality preserved from the GGA (see Invalid/GPS/...).
	FixQuality string
	// DateSource records where the date component came from (e.g. TypeRMC).
	// It is empty when the date could not be resolved.
	DateSource string
	// Sources preserves the original sentence types the fix was combined from,
	// e.g. []string{"GGA", "RMC"}.
	Sources []string
}

// PositionAggregator combines GGA and RMC sentences from the same talker into
// time-referenced position fixes.
//
// Pairing is driven by the real UTC instant (the RMC date together with the
// GGA wall clock), never by arrival order: a late RMC is matched to its GGA as
// long as both are held within the finite window. A dateless GGA that sits
// across midnight from the nearest RMC is not assigned to a day speculatively;
// when the wall-clock gap exceeds the window no date is guessed and no fix is
// produced.
//
// PositionAggregator is safe for concurrent use.
type PositionAggregator struct {
	// Window is the maximum wall-clock difference between a GGA and an RMC for
	// them to be considered the same position epoch. Defaults to 500ms.
	Window time.Duration
	// MaxHold is the maximum time an as-yet unmatched sentence is kept waiting
	// for its counterpart. Defaults to 30s.
	MaxHold time.Duration
	// ReferenceYear is the century reference used to expand the RMC two-digit
	// year into a full year. 0 means the current UTC year.
	ReferenceYear int
	// CoordTolerance is the maximum allowed difference (in decimal degrees)
	// between GGA and RMC coordinates for them to be considered agreeing.
	// Defaults to 1e-5 (roughly one metre).
	CoordTolerance float64

	mu     sync.Mutex
	states map[string]*posState
}

type posState struct {
	gga   *GGA
	ggaAt time.Time
	rmc   *RMC
	rmcAt time.Time
	// lastKey suppresses re-emission of a fix for an already emitted epoch.
	lastKey string
}

// NewPositionAggregator creates a PositionAggregator with the default finite
// windows and coordinate tolerance.
func NewPositionAggregator() *PositionAggregator {
	return &PositionAggregator{
		Window:         500 * time.Millisecond,
		MaxHold:        30 * time.Second,
		CoordTolerance: 1e-5,
	}
}

// Update feeds a parsed sentence into the aggregator. Only GGA and RMC
// sentences are considered; other types are ignored.
//
// It returns a non-nil *PositionFix whenever a GGA+RMC pair is formed. Check
// Valid to know whether the fix is complete and certain; check Uncertain when
// the coordinates conflict. It returns nil when no pair is formed (for example
// a cross-midnight date ambiguity or a missing counterpart).
func (a *PositionAggregator) Update(s Sentence) *PositionFix {
	if s == nil {
		return nil
	}
	now := time.Now()

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.states == nil {
		a.states = make(map[string]*posState)
	}
	st := a.states[s.TalkerID()]
	if st == nil {
		st = &posState{}
		a.states[s.TalkerID()] = st
	}

	switch v := s.(type) {
	case GGA:
		g := v
		st.gga = &g
		st.ggaAt = now
	case RMC:
		r := v
		st.rmc = &r
		st.rmcAt = now
	default:
		return nil
	}

	// Expire sentences that have waited too long for their counterpart.
	if st.gga != nil && now.Sub(st.ggaAt) > a.maxHold() {
		st.gga = nil
	}
	if st.rmc != nil && now.Sub(st.rmcAt) > a.maxHold() {
		st.rmc = nil
	}

	if st.gga == nil || st.rmc == nil {
		return nil
	}

	fix := a.buildFix(st.gga, st.rmc)
	if fix == nil {
		return nil
	}
	key := pairKey(st.gga, st.rmc)
	if key == st.lastKey {
		return nil // already emitted for this epoch
	}
	st.lastKey = key
	return fix
}

// buildFix assembles a PositionFix from a same-talker GGA/RMC pair. It returns
// nil when the pair cannot be placed on a real UTC instant without guessing.
func (a *PositionAggregator) buildFix(g *GGA, r *RMC) *PositionFix {
	// Both sentences must carry a wall clock time.
	if !g.Time.Valid || !r.Time.Valid {
		return nil
	}
	// The date comes from the RMC only. Without it we cannot place the fix on a
	// real UTC instant; do not guess across midnight.
	if !r.Date.Valid {
		return nil
	}
	// Wall-clock gap between the two epochs. A GGA is dateless, so a gap larger
	// than Window means the two sentences may straddle midnight; we never assign
	// a dateless GGA to a day speculatively.
	if wallGap(g.Time, r.Time) > a.window() {
		return nil
	}

	refYear := a.ReferenceYear
	if refYear == 0 {
		refYear = time.Now().UTC().Year()
	}
	instant := DateTime(refYear, r.Date, g.Time)

	fix := &PositionFix{
		Talker:     g.Talker,
		Time:       instant,
		Latitude:   g.Latitude,
		Longitude:  g.Longitude,
		FixQuality: g.FixQuality,
		DateSource: TypeRMC,
		Sources:    []string{TypeGGA, TypeRMC},
	}

	// Invalid fix status is preserved: a valid RMC can never promote an invalid
	// GGA, and an invalid RMC invalidates an otherwise valid GGA.
	ggaValid := g.FixQuality != Invalid
	rmcValid := r.Validity == ValidRMC
	fix.Valid = ggaValid && rmcValid

	// Conflicting coordinates mark the fix uncertain; an uncertain fix is never
	// complete.
	if coordsConflict(g.Latitude, g.Longitude, r.Latitude, r.Longitude, a.coordTolerance()) {
		fix.Uncertain = true
		fix.Valid = false
	}

	return fix
}

func (a *PositionAggregator) window() time.Duration {
	if a.Window > 0 {
		return a.Window
	}
	return 500 * time.Millisecond
}

func (a *PositionAggregator) maxHold() time.Duration {
	if a.MaxHold > 0 {
		return a.MaxHold
	}
	return 30 * time.Second
}

func (a *PositionAggregator) coordTolerance() float64 {
	if a.CoordTolerance > 0 {
		return a.CoordTolerance
	}
	return 1e-5
}

// sinceMidnight returns the wall-clock duration since midnight for t.
func sinceMidnight(t Time) time.Duration {
	return time.Duration(t.Hour)*time.Hour +
		time.Duration(t.Minute)*time.Minute +
		time.Duration(t.Second)*time.Second +
		time.Duration(t.Millisecond)*time.Millisecond
}

// wallGap returns the absolute wall-clock gap between two times of day. It does
// not wrap across midnight, so a dateless GGA near midnight is never paired
// with an RMC on the other side of midnight.
func wallGap(a, b Time) time.Duration {
	d := sinceMidnight(a) - sinceMidnight(b)
	if d < 0 {
		return -d
	}
	return d
}

// coordsConflict reports whether the GGA and RMC coordinates differ by more
// than tol degrees in either component.
func coordsConflict(lat1, lon1, lat2, lon2 float64, tol float64) bool {
	return math.Abs(lat1-lat2) > tol || math.Abs(lon1-lon2) > tol
}

// pairKey identifies a GGA/RMC epoch pair by talker and wall-clock times so a
// fix is emitted only once per epoch.
func pairKey(g *GGA, r *RMC) string {
	return fmt.Sprintf("%s|%s|%s", g.Talker, g.Time.String(), r.Time.String())
}
