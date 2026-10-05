package nmea

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// vdmRaw builds a valid VDM sentence with a correct checksum.
func vdmRaw(total, frag int, msgID interface{}, channel, payload string, fill int) string {
	body := fmt.Sprintf("AIVDM,%d,%d,%v,%s,%s,%d", total, frag, msgID, channel, payload, fill)
	return "!" + body + "*" + Checksum(body)
}

// vdoRaw builds a valid VDO sentence with a correct checksum.
func vdoRaw(total, frag int, msgID interface{}, channel, payload string, fill int) string {
	body := fmt.Sprintf("AIVDO,%d,%d,%v,%s,%s,%d", total, frag, msgID, channel, payload, fill)
	return "!" + body + "*" + Checksum(body)
}

func TestVDMReassembler_ReverseOrder(t *testing.T) {
	// A known single-fragment message, split into two fragments.
	const armor = "13aGt0PP0jPN@9fMPKVDJgwfR>`<"
	fullMsg, err := Parse(vdmRaw(1, 1, "", "A", armor, 0))
	require.NoError(t, err)
	fullVDM := fullMsg.(VDMVDO)

	frag1 := vdmRaw(2, 1, 4, "A", armor[:20], 0)
	frag2 := vdmRaw(2, 2, 4, "A", armor[20:], 0)

	r := NewVDMReassembler(time.Minute, nil)

	// Reverse order: fragment 2 arrives first.
	res, err := r.AddRaw(frag2)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)

	res, err = r.AddRaw(frag1)
	require.NoError(t, err)
	require.NotNil(t, res.Complete)
	assert.Equal(t, armor, res.Complete.Armor)
	assert.Equal(t, fullVDM.Payload, res.Complete.Payload)
	assert.Equal(t, "AI", res.Complete.Talker)
	assert.Equal(t, TypeVDM, res.Complete.Type)
	assert.Equal(t, int64(4), res.Complete.MessageID)
	assert.Equal(t, "A", res.Complete.Channel)
}

func TestVDMReassembler_DuplicateIsIdempotent(t *testing.T) {
	const armor = "13aGt0PP0jPN@9fMPKVDJgwfR>`<"
	frag1 := vdmRaw(2, 1, 4, "A", armor[:20], 0)
	frag2 := vdmRaw(2, 2, 4, "A", armor[20:], 0)

	r := NewVDMReassembler(time.Minute, nil)

	res, err := r.AddRaw(frag1)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)

	// Duplicate of fragment 1: no second output, no error.
	res, err = r.AddRaw(frag1)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)

	res, err = r.AddRaw(frag2)
	require.NoError(t, err)
	require.NotNil(t, res.Complete)

	// Duplicate after completion: no output, no error.
	res, err = r.AddRaw(frag2)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)
}

func TestVDMReassembler_ConflictTotalCount(t *testing.T) {
	frag1 := vdmRaw(2, 1, 4, "A", "00000000000", 0)
	conflict := vdmRaw(3, 1, 4, "A", "00000000000", 0)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag1)
	require.NoError(t, err)

	_, err = r.AddRaw(conflict)
	require.Error(t, err)
	var conflictErr *VDMConflictError
	require.True(t, errors.As(err, &conflictErr))
	assert.Equal(t, int64(-1), conflictErr.Fragment)
	assert.Contains(t, err.Error(), "total fragment count mismatch")
}

func TestVDMReassembler_ConflictPayload(t *testing.T) {
	frag1 := vdmRaw(2, 1, 4, "A", "00000000000", 0)
	conflict := vdmRaw(2, 1, 4, "A", "11111111111", 0)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag1)
	require.NoError(t, err)

	_, err = r.AddRaw(conflict)
	require.Error(t, err)
	var conflictErr *VDMConflictError
	require.True(t, errors.As(err, &conflictErr))
	assert.Equal(t, int64(1), conflictErr.Fragment)
	assert.Contains(t, err.Error(), "payload mismatch")
}

func TestVDMReassembler_MissingFragmentExpiry(t *testing.T) {
	frag1 := vdmRaw(2, 1, 4, "A", "00000000000", 0)

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := NewVDMReassembler(time.Minute, func() time.Time { return now })

	res, err := r.AddRaw(frag1)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)

	// Window has not elapsed yet: no receipts.
	receipts := r.Expire()
	assert.Empty(t, receipts)

	// Advance the clock past the window.
	now = now.Add(2 * time.Minute)
	receipts = r.Expire()
	require.Len(t, receipts, 1)
	assert.Equal(t, "AI", receipts[0].Talker)
	assert.Equal(t, "A", receipts[0].Channel)
	assert.Equal(t, int64(4), receipts[0].MessageID)
	assert.Equal(t, int64(2), receipts[0].Total)
	assert.Equal(t, []int64{1}, receipts[0].Fragments)
	assert.Contains(t, receipts[0].Reason, "window elapsed")
}

func TestVDMReassembler_LazyExpiryOnAdd(t *testing.T) {
	frag1 := vdmRaw(2, 1, 4, "A", "00000000000", 0)

	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	r := NewVDMReassembler(time.Minute, func() time.Time { return now })

	_, err := r.AddRaw(frag1)
	require.NoError(t, err)

	// Advance past the window and add a fragment for a different message.
	now = now.Add(2 * time.Minute)
	other := vdmRaw(1, 1, "", "B", "00000000000", 0)
	res, err := r.AddRaw(other)
	require.NoError(t, err)
	require.NotNil(t, res.Complete)
	// The stale assembly was dropped and reported as expired.
	require.Len(t, res.Expired, 1)
	assert.Equal(t, []int64{1}, res.Expired[0].Fragments)
}

func TestVDMReassembler_EmptySequenceRejected(t *testing.T) {
	// Multi-fragment message with an empty sequence number: cannot pair.
	frag1 := vdmRaw(2, 1, "", "A", "00000000000", 0)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sequence number required")
}

func TestVDMReassembler_NonLastFillBitsRejected(t *testing.T) {
	// Fragment 1 of 2 carries fill bits: rejected.
	frag1 := vdmRaw(2, 1, 4, "A", "00000000000", 2)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fill bits only allowed on last fragment")
}

func TestVDMReassembler_LastFragmentFillBits(t *testing.T) {
	// Only the last fragment may carry fill bits.
	frag1 := vdmRaw(2, 1, 4, "A", "00000000000", 0)
	frag2 := vdmRaw(2, 2, 4, "A", "00000000000", 2)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag1)
	require.NoError(t, err)
	res, err := r.AddRaw(frag2)
	require.NoError(t, err)
	require.NotNil(t, res.Complete)
	// 11 + 11 armor chars = 132 bits, minus 2 fill bits = 130 bits.
	assert.Len(t, res.Complete.Payload, 130)
}

func TestVDMReassembler_SingleFragmentPassThrough(t *testing.T) {
	const armor = "13aGt0PP0jPN@9fMPKVDJgwfR>`<"
	raw := vdmRaw(1, 1, "", "A", armor, 0)

	r := NewVDMReassembler(time.Minute, nil)
	res, err := r.AddRaw(raw)
	require.NoError(t, err)
	require.NotNil(t, res.Complete)
	assert.Equal(t, armor, res.Complete.Armor)
	assert.Equal(t, "AI", res.Complete.Talker)
}

func TestVDMReassembler_InvalidFragmentNumber(t *testing.T) {
	frag := vdmRaw(2, 3, 4, "A", "00000000000", 0)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid fragment number")
}

func TestVDMReassembler_InvalidNumFragments(t *testing.T) {
	frag := vdmRaw(0, 1, 4, "A", "00000000000", 0)

	r := NewVDMReassembler(time.Minute, nil)
	_, err := r.AddRaw(frag)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid number of fragments")
}

func TestVDMReassembler_BindingByTalkerAndChannel(t *testing.T) {
	// Same sequence number but different talker or channel must not pair.
	fragA := vdmRaw(2, 1, 4, "A", "00000000000", 0)
	fragB := vdmRaw(2, 2, 4, "B", "00000000000", 0)

	r := NewVDMReassembler(time.Minute, nil)
	res, err := r.AddRaw(fragA)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)

	// Different channel: staged separately, no completion.
	res, err = r.AddRaw(fragB)
	require.NoError(t, err)
	assert.Nil(t, res.Complete)
}

func TestVDMReassembler_VDO(t *testing.T) {
	frag1 := vdoRaw(2, 1, 4, "A", "00000000000", 0)
	frag2 := vdoRaw(2, 2, 4, "A", "00000000000", 0)

	r := NewVDMReassembler(time.Minute, nil)
	res, err := r.AddRaw(frag1)
	require.NoError(t, err)
	res, err = r.AddRaw(frag2)
	require.NoError(t, err)
	require.NotNil(t, res.Complete)
	assert.Equal(t, TypeVDO, res.Complete.Type)
}

// ExampleVDMReassembler demonstrates reassembling four example sentences:
// two fragments of a split AIS message, a duplicate of the first fragment,
// and a single-fragment message.
func ExampleVDMReassembler() {
	sentences := []string{
		vdmRaw(2, 1, 7, "B", "13aGt0PP0jPN@9fMPKVDJ", 0),
		vdmRaw(2, 1, 7, "B", "13aGt0PP0jPN@9fMPKVDJ", 0), // duplicate of fragment 1
		vdmRaw(2, 2, 7, "B", "00000000000", 2),
		vdmRaw(1, 1, "", "A", "13aGt0PP0jPN@9fMPKVDJgwfR>`<", 0),
	}

	r := NewVDMReassembler(time.Minute, nil)
	for _, raw := range sentences {
		res, err := r.AddRaw(raw)
		if err != nil {
			fmt.Println("error:", err)
			continue
		}
		if res.Complete != nil {
			fmt.Printf("complete: talker=%s type=%s channel=%s id=%d armor=%s bits=%d\n",
				res.Complete.Talker, res.Complete.Type, res.Complete.Channel,
				res.Complete.MessageID, res.Complete.Armor, len(res.Complete.Payload))
		}
	}
	// Output:
	// complete: talker=AI type=VDM channel=B id=7 armor=13aGt0PP0jPN@9fMPKVDJ00000000000 bits=190
	// complete: talker=AI type=VDM channel=A id=0 armor=13aGt0PP0jPN@9fMPKVDJgwfR>`< bits=168
}
