package nmea

import (
	"math"
	"sort"
	"time"
)

const (
	// DefaultPositionMaxLate is the default finite window of stream time
	// within which a late GGA or RMC is still matched to the sentence that
	// arrived earlier for the same talker and UTC time of day. Sentences
	// arriving later than this window are emitted as partial (incomplete)
	// fixes instead of being buffered indefinitely.
	DefaultPositionMaxLate = 30 * time.Second

	// DefaultPositionCoordTolerance is the default maximum distance in
	// meters between the coordinates reported by GGA and RMC for the same
	// UTC time of day before the combined fix is considered conflicting
	// (uncertain).
	DefaultPositionCoordTolerance = 10.0
)

// FixSource identifies one of the NMEA sentences that contributed to a fix.
// The original sentence type (TypeGGA or TypeRMC) is preserved, along with the
// talker id and the raw sentence, so consumers can tell exactly which inputs a
// fix was derived from.
type FixSource struct {
	// Type is the originating sentence type (TypeGGA or TypeRMC).
	Type string
	// Talker is the talker id of the originating sentence (e.g. GP, GN).
	Talker string
	// Raw is the original raw NMEA sentence.
	Raw string
}

// PositionFix is the result of combining a GGA sentence with the RMC sentence
// from the same talker for the same real UTC time of day. A fix is only marked
// Complete (and Valid) when the combining rules establish it with enough
// certainty; otherwise it is emitted as a partial fix with DateKnown/Valid
// left unset and the contributing sentence retained in Sources.
type PositionFix struct {
	// Talker is the talker id the fix was combined for. GGA and RMC with
	// different talkers are never combined.
	Talker string

	// Time is the UTC instant of the fix. It is the zero time.Time unless a
	// dated RMC supplied the date. GGA alone never carries a date, so the
	// date is never guessed (in particular around midnight).
	Time time.Time

	// DateKnown reports whether Time carries a date sourced from an RMC.
	// When false, only the UTC time of day is known.
	DateKnown bool

	// Complete reports that GGA and RMC were combined for the same talker
	// and exact UTC time of day, the date is known and the coordinates did
	// not conflict. Only complete fixes are suitable as full position fixes.
	Complete bool

	// Valid reports that the fix is complete and both inputs mark the
	// position valid: GGA fix quality other than Invalid ("0") and RMC
	// validity ValidRMC ("A"). An invalid RMC can never make a GGA fix
	// valid.
	Valid bool

	// Uncertain reports that the inputs disagreed (for example conflicting
	// coordinates or two possible dates for one time of day). An uncertain
	// fix is never Complete.
	Uncertain bool

	// Stale reports that the fix could not be combined within the finite
	// late-sentence window and is emitted with whichever side arrived.
	Stale bool

	// Latitude/Longitude are the combined coordinates, preferring GGA when
	// both sentences are present.
	Latitude  float64
	Longitude float64

	// Altitude (meters) and fix quality come from GGA when present.
	Altitude   float64
	FixQuality string

	// Speed (knots), course (degrees true) and magnetic variation (degrees,
	// east positive) come from RMC when present.
	Speed     float64
	Course    float64
	Variation float64

	// GGA and RMC are the parsed contributing sentences, nil when that side
	// did not arrive (or arrived too late).
	GGA *GGA
	RMC *RMC

	// Sources lists the originating sentences, deterministically ordered
	// (GGA before RMC) regardless of the arrival order. The original
	// sentence type is preserved on each source.
	Sources []FixSource
}

// PositionAggregator explicitly combines GGA and RMC sentences into position
// fixes. Pairing is never based on arrival order: sentences are matched by the
// pair (talker id, exact UTC time of day). The date of a fix comes only from
// the RMC date field, the GGA/RMC validity states are preserved as parsed,
// late sentences are handled with a finite window and conflicting coordinates
// make the fix uncertain.
//
// PositionAggregator is not safe for concurrent use.
type PositionAggregator struct {
	// ReferenceYear resolves the two-digit RMC year to a full year. Zero
	// means the current UTC year (see DateTime).
	ReferenceYear int

	// MaxLate is the finite window for matching late sentences, measured in
	// the stream's own UTC time (as stated by the sentences, not by the
	// host arrival time). Zero or negative means DefaultPositionMaxLate.
	MaxLate time.Duration

	// CoordTolerance is the maximum allowed distance in meters between GGA
	// and RMC coordinates for the same instant. Zero or negative means
	// DefaultPositionCoordTolerance.
	CoordTolerance float64

	talkers map[string]*talkerFixes
}

// NewPositionAggregator returns a PositionAggregator with default settings.
func NewPositionAggregator() *PositionAggregator {
	return &PositionAggregator{}
}

func (a *PositionAggregator) maxLate() time.Duration {
	if a.MaxLate > 0 {
		return a.MaxLate
	}
	return DefaultPositionMaxLate
}

func (a *PositionAggregator) coordTolerance() float64 {
	if a.CoordTolerance > 0 {
		return a.CoordTolerance
	}
	return DefaultPositionCoordTolerance
}

// todKey identifies a UTC time of day exactly (to the millisecond).
type todKey struct {
	hour, minute, second, millisecond int
}

func todKeyOf(t Time) todKey {
	return todKey{t.Hour, t.Minute, t.Second, t.Millisecond}
}

// fixSlot holds the GGA and RMC seen for one talker and one UTC time of day.
type fixSlot struct {
	key          todKey
	gga          *GGA
	rmc          *RMC
	dateConflict bool
}

// talkerFixes holds the pending slots and the most recent confirmed UTC
// instant of one talker.
type talkerFixes struct {
	slots []*fixSlot
	// clock is the latest UTC instant established by a dated RMC.
	clock      time.Time
	clockValid bool
}

// Add parses a raw NMEA sentence and feeds it to the aggregator when it is a
// GGA or RMC. Other sentence types are ignored. It returns the parser error
// for malformed sentences and any fixes emitted due to combining or expiry.
func (a *PositionAggregator) Add(raw string) ([]PositionFix, error) {
	s, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	return a.AddSentence(s), nil
}

// AddSentence feeds an already parsed GGA or RMC sentence to the aggregator.
// Sentences of other types are ignored. It returns the fixes that became
// certain at this point and any fixes that expired because of the finite
// late-sentence window. The result is independent of the arrival order of the
// GGA and RMC of the same instant.
func (a *PositionAggregator) AddSentence(s Sentence) []PositionFix {
	switch m := s.(type) {
	case GGA:
		return a.addGGA(&m)
	case RMC:
		return a.addRMC(&m)
	default:
		return nil
	}
}

func (a *PositionAggregator) tb(talker string) *talkerFixes {
	if a.talkers == nil {
		a.talkers = map[string]*talkerFixes{}
	}
	b, ok := a.talkers[talker]
	if !ok {
		b = &talkerFixes{}
		a.talkers[talker] = b
	}
	return b
}

func (b *talkerFixes) find(k todKey) *fixSlot {
	for _, s := range b.slots {
		if s.key == k {
			return s
		}
	}
	return nil
}

func (b *talkerFixes) remove(target *fixSlot) {
	out := b.slots[:0]
	for _, s := range b.slots {
		if s != target {
			out = append(out, s)
		}
	}
	b.slots = out
}

func (a *PositionAggregator) addGGA(g *GGA) []PositionFix {
	if !g.Time.Valid {
		// Without a time of day there is no key to locate the matching RMC;
		// never guess which instant an undated GGA belongs to.
		return nil
	}
	b := a.tb(g.Talker)
	k := todKeyOf(g.Time)
	if s := b.find(k); s != nil {
		s.gga = g
		if s.rmc != nil {
			fix := a.build(g.Talker, s)
			b.remove(s)
			return []PositionFix{fix}
		}
		return nil
	}
	// Expire anything older than the finite late window before holding the
	// new GGA. Expiry never attaches a guessed date.
	emitted := a.expire(b, g.Talker)
	b.slots = append(b.slots, &fixSlot{key: k, gga: g})
	return emitted
}

func (a *PositionAggregator) addRMC(r *RMC) []PositionFix {
	if !r.Time.Valid {
		return nil
	}
	b := a.tb(r.Talker)
	k := todKeyOf(r.Time)

	// A dated RMC that falls behind the stream clock by more than the
	// finite late window is a late sentence whose matching GGA can no
	// longer reasonably be expected. Emit it as a stale partial fix instead
	// of buffering it. This deliberately happens before advancing the clock
	// and does not assign a date to any GGA.
	if b.clockValid && r.Date.Valid {
		if instant := DateTime(a.ReferenceYear, r.Date, r.Time); !instant.IsZero() &&
			b.clock.Sub(instant) > a.maxLate() {
			fix := a.build(r.Talker, &fixSlot{key: k, rmc: r})
			fix.Stale = true
			return []PositionFix{fix}
		}
	}

	var emitted []PositionFix
	if s := b.find(k); s != nil {
		if s.rmc != nil && r.Date.Valid && s.rmc.Date.Valid &&
			(r.Date.DD != s.rmc.Date.DD || r.Date.MM != s.rmc.Date.MM || r.Date.YY != s.rmc.Date.YY) {
			// Two dated RMCs for the same talker and time of day claim
			// different dates (the midnight ambiguity). Do not pick one.
			s.dateConflict = true
		}
		s.rmc = r
		if s.gga != nil {
			fix := a.build(r.Talker, s)
			emitted = append(emitted, fix)
			b.remove(s)
		}
	} else {
		emitted = append(emitted, a.expire(b, r.Talker)...)
		b.slots = append(b.slots, &fixSlot{key: k, rmc: r})
	}

	if r.Date.Valid {
		if instant := DateTime(a.ReferenceYear, r.Date, r.Time); !instant.IsZero() &&
			(!b.clockValid || instant.After(b.clock)) {
			b.clock = instant
			b.clockValid = true
		}
	}
	return emitted
}

// expire emits and drops slots that are older than the finite late window
// relative to the latest confirmed UTC instant. Slots without a date are never
// handed one: their time of day is only used to decide whether the mate can
// still arrive, and the emitted fix keeps DateKnown unset.
func (a *PositionAggregator) expire(b *talkerFixes, talker string) []PositionFix {
	if !b.clockValid {
		return nil
	}
	window := a.maxLate()
	var out []PositionFix
	remaining := b.slots[:0]
	for _, s := range b.slots {
		if a.slotExpired(b.clock, s, window) {
			fix := a.build(talker, s)
			fix.Stale = true
			out = append(out, fix)
			continue
		}
		remaining = append(remaining, s)
	}
	b.slots = remaining
	return out
}

// slotExpired reports whether a pending slot can only represent an instant
// more than the window behind the stream clock. Dated slots use their real
// date; undated slots are compared via the latest day placement that is not in
// the future of the clock, so a time of day near midnight is not forced onto
// the previous day.
func (a *PositionAggregator) slotExpired(clock time.Time, s *fixSlot, window time.Duration) bool {
	if s.rmc != nil && s.rmc.Date.Valid {
		instant := DateTime(a.ReferenceYear, s.rmc.Date, s.rmc.Time)
		if instant.IsZero() {
			return false
		}
		return clock.Sub(instant) > window
	}
	today := time.Date(clock.Year(), clock.Month(), clock.Day(),
		s.key.hour, s.key.minute, s.key.second, s.key.millisecond*1e6, time.UTC)
	latest := today
	if today.After(clock) {
		// Today's occurrence is still in the future, so yesterday's
		// occurrence is the latest candidate that could already be known.
		latest = today.AddDate(0, 0, -1)
	}
	return clock.Sub(latest) > window
}

// build combines the contents of a slot into a PositionFix.
func (a *PositionAggregator) build(talker string, s *fixSlot) PositionFix {
	fix := PositionFix{Talker: talker}

	if s.gga != nil {
		fix.GGA = s.gga
		fix.Latitude = s.gga.Latitude
		fix.Longitude = s.gga.Longitude
		fix.Altitude = s.gga.Altitude
		fix.FixQuality = s.gga.FixQuality
		fix.Sources = append(fix.Sources, FixSource{
			Type:   TypeGGA,
			Talker: s.gga.Talker,
			Raw:    s.gga.Raw,
		})
	}
	if s.rmc != nil {
		fix.RMC = s.rmc
		if s.gga == nil {
			fix.Latitude = s.rmc.Latitude
			fix.Longitude = s.rmc.Longitude
		}
		fix.Speed = s.rmc.Speed
		fix.Course = s.rmc.Course
		fix.Variation = s.rmc.Variation
		if s.rmc.Date.Valid {
			if instant := DateTime(a.ReferenceYear, s.rmc.Date, s.rmc.Time); !instant.IsZero() {
				fix.Time = instant
				fix.DateKnown = true
			}
		}
		fix.Sources = append(fix.Sources, FixSource{
			Type:   TypeRMC,
			Talker: s.rmc.Talker,
			Raw:    s.rmc.Raw,
		})
	}

	// A full fix requires both sides for the same talker and exact time of
	// day, a known date, no ambiguity and an RMC that states the position is
	// valid. An invalid RMC means the rules are not certain enough for a
	// complete fix, so it can never make a GGA fix valid.
	fix.Complete = s.gga != nil && s.rmc != nil &&
		fix.DateKnown && !s.dateConflict && s.rmc.Validity == ValidRMC

	if fix.Complete && s.gga.FixQuality != Invalid {
		fix.Valid = true
	}

	if fix.Complete && s.rmc.Validity == ValidRMC &&
		haversine(s.gga.Latitude, s.gga.Longitude,
			s.rmc.Latitude, s.rmc.Longitude) > a.coordTolerance() {
		fix.Uncertain = true
		fix.Complete = false
		fix.Valid = false
	}
	if s.dateConflict {
		fix.Uncertain = true
		fix.Complete = false
		fix.Valid = false
	}
	return fix
}

// Flush emits all pending partial fixes (sentences whose mate never arrived
// within the finite window) and empties the aggregator. Talkers are processed
// in lexical order and slots in insertion order, so the result is deterministic.
func (a *PositionAggregator) Flush() []PositionFix {
	talkers := make([]string, 0, len(a.talkers))
	for t := range a.talkers {
		talkers = append(talkers, t)
	}
	sort.Strings(talkers)
	var out []PositionFix
	for _, t := range talkers {
		b := a.talkers[t]
		for _, s := range b.slots {
			out = append(out, a.build(t, s))
		}
		b.slots = nil
	}
	a.talkers = map[string]*talkerFixes{}
	return out
}

// haversine returns the great-circle distance in meters between two WGS-84
// coordinates.
func haversine(lat1, lon1, lat2, lon2 float64) float64 {
	const earthRadiusMeters = 6371000.0
	lat1r := lat1 * math.Pi / 180
	lat2r := lat2 * math.Pi / 180
	dlat := (lat2 - lat1) * math.Pi / 180
	dlon := (lon2 - lon1) * math.Pi / 180
	h := math.Sin(dlat/2)*math.Sin(dlat/2) +
		math.Cos(lat1r)*math.Cos(lat2r)*math.Sin(dlon/2)*math.Sin(dlon/2)
	return 2 * earthRadiusMeters * math.Asin(math.Sqrt(h))
}
