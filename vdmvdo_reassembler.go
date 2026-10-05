package nmea

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultVDMVDOReassemblyWindow is the default time window during which all
// fragments of a multi-fragment VDM/VDO message must arrive. When the window
// expires the incomplete message is dropped and reported with an explicit
// expired receipt; its partial payload is never handed to the caller.
const DefaultVDMVDOReassemblyWindow = 30 * time.Second

// Reassembly rejection reasons returned by VDMVDOReassembler.Add.
var (
	// ErrVDMVDOUnexpectedType is returned when the sentence fed to the
	// reassembler is not a VDM or VDO sentence.
	ErrVDMVDOUnexpectedType = errors.New("nmea: VDM/VDO reassembly: sentence is not VDM or VDO")
	// ErrVDMVDOMissingSequence is returned when a multi-fragment message
	// carries an empty sequence (message ID) field. Empty sequence numbers
	// are never guessed or paired up.
	ErrVDMVDOMissingSequence = errors.New("nmea: VDM/VDO reassembly: multi-fragment message without sequence number")
	// ErrVDMVDOFragmentRange is returned when the fragment number is not
	// within 1..NumFragments.
	ErrVDMVDOFragmentRange = errors.New("nmea: VDM/VDO reassembly: fragment number out of range")
	// ErrVDMVDOCountConflict is returned when fragments bound to the same
	// key disagree about the total fragment count.
	ErrVDMVDOCountConflict = errors.New("nmea: VDM/VDO reassembly: conflicting fragment count")
	// ErrVDMVDOFragmentConflict is returned when a fragment number is
	// received twice with different payload or fill bits.
	ErrVDMVDOFragmentConflict = errors.New("nmea: VDM/VDO reassembly: conflicting fragment payload")
	// ErrVDMVDOFillBits is returned when a non-final fragment carries fill
	// (padding) bits. Only the final fragment may be padded.
	ErrVDMVDOFillBits = errors.New("nmea: VDM/VDO reassembly: non-final fragment carries fill bits")
)

// VDMVDOKey binds the fragments of one multi-fragment message together.
// Fragments are only reassembled when talker, sequence number (message ID)
// and radio channel all match.
type VDMVDOKey struct {
	Talker    string
	MessageID int64
	Channel   string
}

// VDMVDOReceiptStatus describes the outcome of feeding a fragment to the
// reassembler (or of a reassembly window expiring).
type VDMVDOReceiptStatus int

const (
	// VDMVDOReceiptPending means the fragment was buffered and more
	// fragments are needed to complete the message.
	VDMVDOReceiptPending VDMVDOReceiptStatus = iota
	// VDMVDOReceiptComplete means the message is complete; Payload and
	// Armored are populated exactly once per message.
	VDMVDOReceiptComplete
	// VDMVDOReceiptDuplicate means the exact same fragment (identical
	// payload) was already seen; it is an idempotent no-op, no output.
	VDMVDOReceiptDuplicate
	// VDMVDOReceiptExpired means the reassembly window expired before the
	// message was complete. The partial payload is discarded, never emitted.
	VDMVDOReceiptExpired
)

// String returns a human readable name of the receipt status.
func (s VDMVDOReceiptStatus) String() string {
	switch s {
	case VDMVDOReceiptPending:
		return "pending"
	case VDMVDOReceiptComplete:
		return "complete"
	case VDMVDOReceiptDuplicate:
		return "duplicate"
	case VDMVDOReceiptExpired:
		return "expired"
	}
	return "unknown"
}

// VDMVDOReceipt is the explicit receipt returned for every fragment fed to
// the reassembler and for every expired reassembly window. Payload and
// Armored are only populated when Status is VDMVDOReceiptComplete; a partial
// (half) payload is never exposed to the caller.
type VDMVDOReceipt struct {
	Status   VDMVDOReceiptStatus
	Key      VDMVDOKey
	Received int    // number of distinct fragments currently held
	Total    int    // expected total fragment count
	Armored  string // reassembled 6-bit armored payload, only when Complete
	Payload  []byte // de-armored bits (one byte per bit), only when Complete
}

// vdmvdoFragment is a single buffered fragment.
type vdmvdoFragment struct {
	armored  string
	fillBits int
}

// vdmvdoAssembly is the in-flight reassembly state of one message key.
type vdmvdoAssembly struct {
	total   int
	frags   map[int]vdmvdoFragment
	done    bool // completed; kept until expiry so duplicates stay idempotent
	expires time.Time
}

// VDMVDOReassemblerOption configures a VDMVDOReassembler.
type VDMVDOReassemblerOption func(*VDMVDOReassembler)

// WithVDMVDOReassemblyWindow sets the reassembly window (time to live of an
// incomplete message). Defaults to DefaultVDMVDOReassemblyWindow.
func WithVDMVDOReassemblyWindow(d time.Duration) VDMVDOReassemblerOption {
	return func(r *VDMVDOReassembler) {
		if d > 0 {
			r.window = d
		}
	}
}

// WithVDMVDOReassemblyClock injects the clock used for window expiry.
// Defaults to time.Now. Useful for deterministic tests.
func WithVDMVDOReassemblyClock(now func() time.Time) VDMVDOReassemblerOption {
	return func(r *VDMVDOReassembler) {
		if now != nil {
			r.now = now
		}
	}
}

// VDMVDOReassembler explicitly reassembles multi-fragment NMEA VDM/VDO
// sentences. Fragments are bound together by talker, sequence number and
// channel; out-of-order fragments are buffered until the message completes.
// Only the armored payload content is reassembled and de-armored; the
// encapsulated vessel (AIS) data is never interpreted.
//
// Example message (two fragments):
//
//	!AIVDM,2,1,7,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0*15
//	!AIVDM,2,2,7,B,0000000000,2*10
//
// VDMVDOReassembler is not co-routine safe.
type VDMVDOReassembler struct {
	window  time.Duration
	now     func() time.Time
	pending map[VDMVDOKey]*vdmvdoAssembly
	expired []VDMVDOReceipt
}

// NewVDMVDOReassembler creates a reassembler with the given options.
func NewVDMVDOReassembler(opts ...VDMVDOReassemblerOption) *VDMVDOReassembler {
	r := &VDMVDOReassembler{
		window:  DefaultVDMVDOReassemblyWindow,
		now:     time.Now,
		pending: make(map[VDMVDOKey]*vdmvdoAssembly),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Add feeds one raw VDM/VDO sentence to the reassembler and returns an
// explicit receipt for it. Fragments that violate the reassembly rules
// (count conflict, fragment conflict, non-final fill bits, missing sequence
// number, out-of-range fragment number) are rejected with an error and do
// not mutate the reassembly state.
func (r *VDMVDOReassembler) Add(raw string) (VDMVDOReceipt, error) {
	r.purge(r.now())

	var base BaseSentence
	sp := SentenceParser{
		OnBaseSentence: func(s *BaseSentence) error { base = *s; return nil },
	}
	sent, err := sp.Parse(raw)
	if err != nil {
		return VDMVDOReceipt{}, err
	}
	m, ok := sent.(VDMVDO)
	if !ok {
		return VDMVDOReceipt{}, fmt.Errorf("%w: %s", ErrVDMVDOUnexpectedType, base.Prefix())
	}
	if len(base.Fields) < 6 {
		return VDMVDOReceipt{}, fmt.Errorf("nmea: %s invalid VDM/VDO reassembly: too few fields", base.Prefix())
	}

	var (
		total   = int(m.NumFragments)
		num     = int(m.FragmentNumber)
		seqRaw  = base.Fields[2]
		armored = base.Fields[4]
	)
	fillBits, err := strconv.Atoi(base.Fields[5])
	if err != nil {
		return VDMVDOReceipt{}, fmt.Errorf("nmea: %s invalid VDM/VDO reassembly fill bits: %s", base.Prefix(), base.Fields[5])
	}

	key := VDMVDOKey{Talker: base.Talker, MessageID: m.MessageID, Channel: m.Channel}

	if total < 1 || num < 1 || num > total {
		return VDMVDOReceipt{}, fmt.Errorf("%w: fragment %d of %d", ErrVDMVDOFragmentRange, num, total)
	}
	if total > 1 && seqRaw == "" {
		// An empty sequence number must not be guessed or paired up.
		return VDMVDOReceipt{}, ErrVDMVDOMissingSequence
	}
	if total > 1 && num < total && fillBits != 0 {
		return VDMVDOReceipt{}, fmt.Errorf("%w: fragment %d of %d has %d fill bits", ErrVDMVDOFillBits, num, total, fillBits)
	}

	// Single-fragment messages complete immediately, no state is kept.
	if total == 1 {
		payload, derr := decodeSixBitASCIIArmour(armored, fillBits)
		if derr != nil {
			return VDMVDOReceipt{}, fmt.Errorf("nmea: %s invalid VDM/VDO reassembly payload: %s", base.Prefix(), derr)
		}
		return VDMVDOReceipt{
			Status:   VDMVDOReceiptComplete,
			Key:      key,
			Received: 1,
			Total:    1,
			Armored:  armored,
			Payload:  payload,
		}, nil
	}

	a, ok := r.pending[key]
	if ok {
		if a.total != total {
			return VDMVDOReceipt{}, fmt.Errorf("%w: key %+v declares %d, already have %d", ErrVDMVDOCountConflict, key, total, a.total)
		}
		if f, dup := a.frags[num]; dup {
			if f.armored == armored && f.fillBits == fillBits {
				// Identical retransmission: idempotent no-op, no output.
				return VDMVDOReceipt{Status: VDMVDOReceiptDuplicate, Key: key, Received: len(a.frags), Total: a.total}, nil
			}
			return VDMVDOReceipt{}, fmt.Errorf("%w: key %+v fragment %d", ErrVDMVDOFragmentConflict, key, num)
		}
	} else {
		a = &vdmvdoAssembly{total: total, frags: make(map[int]vdmvdoFragment)}
		r.pending[key] = a
	}

	a.frags[num] = vdmvdoFragment{armored: armored, fillBits: fillBits}
	a.expires = r.now().Add(r.window)

	if len(a.frags) < a.total {
		return VDMVDOReceipt{Status: VDMVDOReceiptPending, Key: key, Received: len(a.frags), Total: a.total}, nil
	}

	// All fragments arrived (in any order): concatenate the armored content
	// in fragment order and de-armor once with the final fragment's fill bits.
	var sb strings.Builder
	for i := 1; i <= a.total; i++ {
		sb.WriteString(a.frags[i].armored)
	}
	joined := sb.String()
	payload, derr := decodeSixBitASCIIArmour(joined, a.frags[a.total].fillBits)
	if derr != nil {
		delete(r.pending, key)
		return VDMVDOReceipt{}, fmt.Errorf("nmea: %s invalid VDM/VDO reassembly payload: %s", base.Prefix(), derr)
	}
	// Keep the completed assembly until its window expires so that
	// retransmitted fragments stay idempotent and never emit the payload twice.
	a.done = true
	return VDMVDOReceipt{
		Status:   VDMVDOReceiptComplete,
		Key:      key,
		Received: a.total,
		Total:    a.total,
		Armored:  joined,
		Payload:  payload,
	}, nil
}

// Expired purges assemblies whose reassembly window has expired and returns
// one explicit VDMVDOReceiptExpired receipt per incomplete message. Their
// partial payloads are discarded and never returned. Completed assemblies
// are purged silently. Receipts are returned in a deterministic key order.
func (r *VDMVDOReassembler) Expired() []VDMVDOReceipt {
	r.purge(r.now())
	expired := r.expired
	r.expired = nil
	return expired
}

// Pending returns the number of message keys currently held (complete or
// incomplete), mainly useful for tests and monitoring.
func (r *VDMVDOReassembler) Pending() int {
	r.purge(r.now())
	return len(r.pending)
}

// purge drops expired assemblies, queueing receipts for incomplete ones.
func (r *VDMVDOReassembler) purge(now time.Time) {
	var keys []VDMVDOKey
	for k, a := range r.pending {
		if !now.Before(a.expires) {
			if !a.done {
				r.expired = append(r.expired, VDMVDOReceipt{
					Status:   VDMVDOReceiptExpired,
					Key:      k,
					Received: len(a.frags),
					Total:    a.total,
				})
			}
			keys = append(keys, k)
		}
	}
	for _, k := range keys {
		delete(r.pending, k)
	}
	if len(r.expired) > 1 {
		sort.Slice(r.expired, func(i, j int) bool {
			a, b := r.expired[i].Key, r.expired[j].Key
			if a.Talker != b.Talker {
				return a.Talker < b.Talker
			}
			if a.MessageID != b.MessageID {
				return a.MessageID < b.MessageID
			}
			return a.Channel < b.Channel
		})
	}
}
