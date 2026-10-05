package nmea

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// VDMMessage is a completely reassembled VDM/VDO message. The reassembler
// only concatenates the armored payload fragments; it never interprets the
// vessel data carried by the payload.
type VDMMessage struct {
	Talker    string
	Type      string // TypeVDM or TypeVDO
	MessageID int64
	Channel   string
	Armor     string // reassembled 6-bit ASCII armor
	Payload   []byte // decoded bits (MSB first)
}

// VDMIncomplete is a receipt for an assembly that never completed within the
// reassembly window.
type VDMIncomplete struct {
	Talker    string
	Channel   string
	MessageID int64
	Total     int64
	Fragments []int64 // fragment numbers received, sorted ascending
	Reason    string
}

// VDMConflictError indicates a fragment that conflicts with a previously
// staged fragment of the same message.
type VDMConflictError struct {
	Talker    string
	Channel   string
	MessageID int64
	Fragment  int64 // -1 when the total fragment count conflicts
	Reason    string
}

// Error returns the error message.
func (e *VDMConflictError) Error() string {
	if e.Fragment < 0 {
		return fmt.Sprintf("nmea: VDM reassembly conflict: talker %s channel %s message %d: %s",
			e.Talker, e.Channel, e.MessageID, e.Reason)
	}
	return fmt.Sprintf("nmea: VDM reassembly conflict: talker %s channel %s message %d fragment %d: %s",
		e.Talker, e.Channel, e.MessageID, e.Fragment, e.Reason)
}

// VDMAddResult is the outcome of feeding one fragment to the reassembler.
type VDMAddResult struct {
	// Complete is set when the added fragment completed a message.
	Complete *VDMMessage
	// Expired lists incomplete assemblies that were dropped because their
	// reassembly window elapsed.
	Expired []*VDMIncomplete
}

type vdmKey struct {
	talker    string
	messageID int64
	channel   string
}

type vdmFragment struct {
	armor    string
	fillBits int
}

type vdmAssembly struct {
	total     int64
	fragments map[int64]vdmFragment // fragment number -> fragment
	first     time.Time
}

// VDMReassembler reassembles split VDM/VDO sentences. Fragments are bound by
// talker, sequence number and channel. Out-of-order fragments are staged
// until the message completes or the reassembly window elapses.
//
// VDMReassembler is safe for concurrent use.
type VDMReassembler struct {
	mu      sync.Mutex
	window  time.Duration
	now     func() time.Time
	pending map[vdmKey]*vdmAssembly
}

// NewVDMReassembler creates a reassembler with the given reassembly window and
// clock. When now is nil, time.Now is used.
func NewVDMReassembler(window time.Duration, now func() time.Time) *VDMReassembler {
	if now == nil {
		now = time.Now
	}
	return &VDMReassembler{
		window:  window,
		now:     now,
		pending: map[vdmKey]*vdmAssembly{},
	}
}

// Add feeds a parsed VDM/VDO fragment to the reassembler. A fragment that
// completes a message returns the reassembled message. Duplicate fragments
// (same fragment number and identical payload) are idempotent. Conflicting
// fragments and invalid fragments are rejected with an error.
func (r *VDMReassembler) Add(m VDMVDO) (VDMAddResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	result := VDMAddResult{}
	result.Expired = r.expireLocked(r.now())

	if err := r.validate(m); err != nil {
		return result, err
	}

	// Single-fragment messages need no pairing; pass through directly.
	if m.NumFragments == 1 {
		result.Complete = &VDMMessage{
			Talker:    m.Talker,
			Type:      m.Type,
			MessageID: m.MessageID,
			Channel:   m.Channel,
			Armor:     m.Fields[4],
			Payload:   m.Payload,
		}
		return result, nil
	}

	key := vdmKey{talker: m.Talker, messageID: m.MessageID, channel: m.Channel}
	frag := vdmFragment{armor: m.Fields[4], fillBits: vdmFillBits(m)}

	assembly, ok := r.pending[key]
	if ok && assembly.total != m.NumFragments {
		return result, &VDMConflictError{
			Talker: m.Talker, Channel: m.Channel, MessageID: m.MessageID,
			Fragment: -1,
			Reason:  fmt.Sprintf("total fragment count mismatch: got %d, want %d", m.NumFragments, assembly.total),
		}
	}
	if !ok {
		assembly = &vdmAssembly{
			total:     m.NumFragments,
			fragments: map[int64]vdmFragment{},
			first:     r.now(),
		}
		r.pending[key] = assembly
	}

	// Duplicate fragment: idempotent only when payload is identical.
	if existing, dup := assembly.fragments[m.FragmentNumber]; dup {
		if existing.armor != frag.armor {
			return result, &VDMConflictError{
				Talker: m.Talker, Channel: m.Channel, MessageID: m.MessageID,
				Fragment: m.FragmentNumber,
				Reason:  "payload mismatch for duplicate fragment",
			}
		}
		return result, nil
	}

	assembly.fragments[m.FragmentNumber] = frag

	// Complete once every fragment has arrived.
	if int64(len(assembly.fragments)) == assembly.total {
		armors := make([]string, assembly.total)
		for i := int64(1); i <= assembly.total; i++ {
			armors[i-1] = assembly.fragments[i].armor
		}
		armorAll := strings.Join(armors, "")
		fillBits := assembly.fragments[assembly.total].fillBits
		payload, err := DecodeSixBitASCII(armorAll, fillBits)
		if err != nil {
			return result, fmt.Errorf("nmea: VDM reassembly: %s", err.Error())
		}
		result.Complete = &VDMMessage{
			Talker:    m.Talker,
			Type:      m.Type,
			MessageID: m.MessageID,
			Channel:   m.Channel,
			Armor:     armorAll,
			Payload:   payload,
		}
		delete(r.pending, key)
	}

	return result, nil
}

// AddRaw parses a raw NMEA sentence and feeds it to the reassembler.
func (r *VDMReassembler) AddRaw(raw string) (VDMAddResult, error) {
	s, err := Parse(raw)
	if err != nil {
		return VDMAddResult{}, err
	}
	m, ok := s.(VDMVDO)
	if !ok {
		return VDMAddResult{}, fmt.Errorf("nmea: %q is not a VDM/VDO sentence", raw)
	}
	return r.Add(m)
}

// Expire drops incomplete assemblies whose reassembly window elapsed and
// returns a receipt for each dropped assembly.
func (r *VDMReassembler) Expire() []*VDMIncomplete {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expireLocked(r.now())
}

func (r *VDMReassembler) validate(m VDMVDO) error {
	if m.NumFragments < 1 {
		return fmt.Errorf("nmea: VDM reassembly: invalid number of fragments: %d", m.NumFragments)
	}
	if m.FragmentNumber < 1 || m.FragmentNumber > m.NumFragments {
		return fmt.Errorf("nmea: VDM reassembly: invalid fragment number: %d", m.FragmentNumber)
	}
	if m.NumFragments > 1 {
		// Multi-fragment messages need a sequence number to bind fragments.
		// An empty sequence number cannot be used to guess a pairing.
		if len(m.Fields) < 3 || m.Fields[2] == "" {
			return fmt.Errorf("nmea: VDM reassembly: sequence number required for multi-fragment message")
		}
		// Fill bits are only allowed on the last fragment.
		if m.FragmentNumber != m.NumFragments && vdmFillBits(m) != 0 {
			return fmt.Errorf("nmea: VDM reassembly: fill bits only allowed on last fragment")
		}
	}
	return nil
}

func (r *VDMReassembler) expireLocked(now time.Time) []*VDMIncomplete {
	if len(r.pending) == 0 {
		return nil
	}
	var expired []*VDMIncomplete
	for key, a := range r.pending {
		if now.Sub(a.first) <= r.window {
			continue
		}
		frags := make([]int64, 0, len(a.fragments))
		for fn := range a.fragments {
			frags = append(frags, fn)
		}
		sort.Slice(frags, func(i, j int) bool { return frags[i] < frags[j] })
		expired = append(expired, &VDMIncomplete{
			Talker:    key.talker,
			Channel:   key.channel,
			MessageID: key.messageID,
			Total:     a.total,
			Fragments: frags,
			Reason:    "reassembly window elapsed",
		})
		delete(r.pending, key)
	}
	sort.Slice(expired, func(i, j int) bool {
		if expired[i].Talker != expired[j].Talker {
			return expired[i].Talker < expired[j].Talker
		}
		if expired[i].Channel != expired[j].Channel {
			return expired[i].Channel < expired[j].Channel
		}
		return expired[i].MessageID < expired[j].MessageID
	})
	return expired
}

// vdmFillBits returns the number of fill bits of a parsed VDM/VDO sentence.
func vdmFillBits(m VDMVDO) int {
	if len(m.Fields) > 5 && m.Fields[5] != "" {
		n, _ := strconv.Atoi(m.Fields[5])
		return n
	}
	return 0
}
