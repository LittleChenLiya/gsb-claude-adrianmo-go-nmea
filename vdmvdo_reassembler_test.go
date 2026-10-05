package nmea

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vdmReasmRaw builds a raw sentence with a valid checksum.
func vdmReasmRaw(start, body string) string {
	return start + body + ChecksumSep + Checksum(body)
}

// Example sentences used by the reassembler tests (all checksums valid):
//
//	!AIVDM,2,1,7,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0*15  (fragment 1 of 2)
//	!AIVDM,2,2,7,B,0000000000,2*10                                (fragment 2 of 2)
//	!AIVDM,1,1,,A,13aGt0PP0jPN@9fMPKVDJgwfR>`<,0*55               (single fragment)
//	!AIVDM,1,1,,A,H77nSfPh4U=<E`H4U8G;:222220,2*1F                (single fragment, padded)
const (
	vdmReasmFrag1 = "!AIVDM,2,1,7,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0*15"
	vdmReasmFrag2 = "!AIVDM,2,2,7,B,0000000000,2*10"
)

// TestVDMVDOReassemblerOutOfOrder verifies that a two-fragment message
// delivered in reverse order yields the complete payload exactly once, and
// that no partial payload is exposed before completion.
func TestVDMVDOReassemblerOutOfOrder(t *testing.T) {
	r := NewVDMVDOReassembler()

	// Second fragment arrives first: buffered, no payload yet.
	rec, err := r.Add(vdmReasmFrag2)
	require.NoError(t, err)
	assert.Equal(t, VDMVDOReceiptPending, rec.Status)
	assert.Equal(t, 1, rec.Received)
	assert.Equal(t, 2, rec.Total)
	assert.Empty(t, rec.Armored)
	assert.Nil(t, rec.Payload)

	// First fragment completes the message.
	rec, err = r.Add(vdmReasmFrag1)
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.Equal(t, VDMVDOKey{Talker: "AI", MessageID: 7, Channel: "B"}, rec.Key)
	assert.Equal(t,
		"55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o"+"0000000000",
		rec.Armored)

	// Cross-check the de-armored payload against the equivalent
	// single-fragment sentence parsed by the regular VDMVDO parser.
	want, err := Parse(vdmReasmRaw("!", "AIVDM,1,1,,B,"+rec.Armored+",2"))
	require.NoError(t, err)
	assert.Equal(t, want.(VDMVDO).Payload, rec.Payload)
	assert.NotEmpty(t, rec.Payload)
}

// TestVDMVDOReassemblerDuplicateFragment verifies that re-sending the exact
// same fragment is an idempotent no-op and never produces a second output.
func TestVDMVDOReassemblerDuplicateFragment(t *testing.T) {
	r := NewVDMVDOReassembler()

	rec, err := r.Add(vdmReasmFrag1)
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptPending, rec.Status)

	// Same fragment, same payload: idempotent duplicate, still pending.
	rec, err = r.Add(vdmReasmFrag1)
	require.NoError(t, err)
	assert.Equal(t, VDMVDOReceiptDuplicate, rec.Status)
	assert.Empty(t, rec.Armored)
	assert.Nil(t, rec.Payload)

	// Completing fragment: exactly one complete receipt.
	rec, err = r.Add(vdmReasmFrag2)
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.NotEmpty(t, rec.Payload)

	// Retransmissions after completion stay idempotent: no second payload.
	for _, raw := range []string{vdmReasmFrag1, vdmReasmFrag2} {
		rec, err = r.Add(raw)
		require.NoError(t, err)
		assert.Equal(t, VDMVDOReceiptDuplicate, rec.Status)
		assert.Empty(t, rec.Armored)
		assert.Nil(t, rec.Payload)
	}
}

// TestVDMVDOReassemblerConflictingFragment verifies that the same fragment
// number with a different payload is rejected.
func TestVDMVDOReassemblerConflictingFragment(t *testing.T) {
	r := NewVDMVDOReassembler()

	_, err := r.Add(vdmReasmFrag1)
	require.NoError(t, err)

	// Same key (talker AI, seq 7, channel B), same fragment number 1,
	// different payload: conflict, not idempotent.
	_, err = r.Add(vdmReasmRaw("!", "AIVDM,2,1,7,B,1111111111,0"))
	assert.True(t, errors.Is(err, ErrVDMVDOFragmentConflict))
}

// TestVDMVDOReassemblerCountConflict verifies that fragments bound to the
// same key but disagreeing on the total fragment count are rejected.
func TestVDMVDOReassemblerCountConflict(t *testing.T) {
	r := NewVDMVDOReassembler()

	_, err := r.Add(vdmReasmFrag1) // declares total 2
	require.NoError(t, err)

	_, err = r.Add(vdmReasmRaw("!", "AIVDM,3,1,7,B,0000000000,0")) // declares total 3
	assert.True(t, errors.Is(err, ErrVDMVDOCountConflict))
}

// TestVDMVDOReassemblerNonFinalFillBits verifies that a non-final fragment
// carrying fill bits is rejected.
func TestVDMVDOReassemblerNonFinalFillBits(t *testing.T) {
	r := NewVDMVDOReassembler()

	_, err := r.Add(vdmReasmRaw("!", "AIVDM,2,1,8,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,1"))
	assert.True(t, errors.Is(err, ErrVDMVDOFillBits))
}

// TestVDMVDOReassemblerMissingSequence verifies that a multi-fragment
// message with an empty sequence number is rejected instead of being
// guessed/paired, while a single-fragment message with empty sequence
// number is fine.
func TestVDMVDOReassemblerMissingSequence(t *testing.T) {
	r := NewVDMVDOReassembler()

	_, err := r.Add(vdmReasmRaw("!", "AIVDM,2,1,,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0"))
	assert.True(t, errors.Is(err, ErrVDMVDOMissingSequence))

	rec, err := r.Add("!AIVDM,1,1,,A,13aGt0PP0jPN@9fMPKVDJgwfR>`<,0*55")
	require.NoError(t, err)
	assert.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.NotEmpty(t, rec.Payload)
}

// TestVDMVDOReassemblerFragmentRange verifies out-of-range fragment numbers
// are rejected.
func TestVDMVDOReassemblerFragmentRange(t *testing.T) {
	r := NewVDMVDOReassembler()

	_, err := r.Add(vdmReasmRaw("!", "AIVDM,2,3,7,B,0000000000,2"))
	assert.True(t, errors.Is(err, ErrVDMVDOFragmentRange))
}

// TestVDMVDOReassemblerWindowExpiry verifies that an incomplete message
// whose reassembly window expires produces an explicit expired receipt and
// that its partial payload is never handed out. Uses an injected clock.
func TestVDMVDOReassemblerWindowExpiry(t *testing.T) {
	now := time.Unix(1700000000, 0)
	r := NewVDMVDOReassembler(
		WithVDMVDOReassemblyWindow(time.Minute),
		WithVDMVDOReassemblyClock(func() time.Time { return now }),
	)

	rec, err := r.Add(vdmReasmFrag1)
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptPending, rec.Status)
	assert.Equal(t, 1, r.Pending())

	// Nothing expired yet.
	assert.Empty(t, r.Expired())

	// Advance beyond the window: explicit expired receipt, no half payload.
	now = now.Add(2 * time.Minute)
	expired := r.Expired()
	require.Len(t, expired, 1)
	assert.Equal(t, VDMVDOReceiptExpired, expired[0].Status)
	assert.Equal(t, VDMVDOKey{Talker: "AI", MessageID: 7, Channel: "B"}, expired[0].Key)
	assert.Equal(t, 1, expired[0].Received)
	assert.Equal(t, 2, expired[0].Total)
	assert.Empty(t, expired[0].Armored)
	assert.Nil(t, expired[0].Payload)
	assert.Equal(t, 0, r.Pending())

	// Receipts are drained once.
	assert.Empty(t, r.Expired())

	// The same message can be reassembled from scratch after expiry.
	rec, err = r.Add(vdmReasmFrag1)
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptPending, rec.Status)
	rec, err = r.Add(vdmReasmFrag2)
	require.NoError(t, err)
	assert.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.NotEmpty(t, rec.Payload)
}

// TestVDMVDOReassemblerKeyBinding verifies that fragments are bound by
// talker, sequence number and channel: same sequence on another channel or
// talker forms an independent assembly.
func TestVDMVDOReassemblerKeyBinding(t *testing.T) {
	r := NewVDMVDOReassembler()

	// Same seq 7 on channel A and channel B, plus talker AB on channel B.
	for _, body := range []string{
		"AIVDM,2,1,7,A,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0",
		"AIVDM,2,1,7,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0",
		"ABVDM,2,1,7,B,55?MbV02;H;s<HtKR20EHE:0@T4@Dn2222222216o,0",
	} {
		rec, err := r.Add(vdmReasmRaw("!", body))
		require.NoError(t, err)
		require.Equal(t, VDMVDOReceiptPending, rec.Status)
	}
	assert.Equal(t, 3, r.Pending())

	// Completing the (AI, 7, B) message leaves the others pending. The
	// completed key is retained until its window expires so that
	// retransmitted fragments stay idempotent.
	rec, err := r.Add(vdmReasmFrag2)
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.Equal(t, VDMVDOKey{Talker: "AI", MessageID: 7, Channel: "B"}, rec.Key)
	assert.Equal(t, 3, r.Pending())

	// The (AI, 7, A) message is still independent and completes on its own.
	rec, err = r.Add(vdmReasmRaw("!", "AIVDM,2,2,7,A,0000000000,2"))
	require.NoError(t, err)
	require.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.Equal(t, VDMVDOKey{Talker: "AI", MessageID: 7, Channel: "A"}, rec.Key)
}

// TestVDMVDOReassemblerSingleFragment verifies single-fragment VDM/VDO
// sentences complete immediately without keeping state.
func TestVDMVDOReassemblerSingleFragment(t *testing.T) {
	r := NewVDMVDOReassembler()

	rec, err := r.Add("!AIVDM,1,1,,A,H77nSfPh4U=<E`H4U8G;:222220,2*1F")
	require.NoError(t, err)
	assert.Equal(t, VDMVDOReceiptComplete, rec.Status)
	assert.Equal(t, "H77nSfPh4U=<E`H4U8G;:222220", rec.Armored)
	assert.NotEmpty(t, rec.Payload)
	assert.Equal(t, 0, r.Pending())

	rec, err = r.Add(vdmReasmRaw("!", "AIVDO,1,1,,B,H77nSfPh4U=<E`H4U8G;:222220,2"))
	require.NoError(t, err)
	assert.Equal(t, VDMVDOReceiptComplete, rec.Status)
}

// TestVDMVDOReassemblerRejectsNonVDM verifies non VDM/VDO sentences are
// rejected.
func TestVDMVDOReassemblerRejectsNonVDM(t *testing.T) {
	r := NewVDMVDOReassembler()

	_, err := r.Add(vdmReasmRaw("$", "GPTXT,01,01,02,hello"))
	assert.True(t, errors.Is(err, ErrVDMVDOUnexpectedType))

	_, err = r.Add("not a sentence")
	assert.Error(t, err)
}
