package server

import (
	"encoding/base64"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testTaskSealer is a sealer with a fixed key and epoch, so a test can build a
// second one that differs from it in exactly one input.
func testTaskSealer(epoch byte, namespace string) *taskSealer {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	t := &taskSealer{key: key, namespace: namespace}
	t.epoch[0] = epoch
	return t
}

// rawTaskID decodes a sealed id, so a test can alter one field of it.
func rawTaskID(t *testing.T, id string) []byte {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(id)
	require.NoError(t, err)
	return raw
}

// resealTaskID re-authenticates altered bytes with s, so that only the
// altered field can make open refuse them.
func resealTaskID(s *taskSealer, raw []byte, caller string) string {
	body := raw[:len(raw)-taskIDMACLen]
	mac := s.mac(body, caller)
	return base64.RawURLEncoding.EncodeToString(append(slices.Clip(body), mac[:taskIDMACLen]...))
}

// flipTrailingBits changes the unused low bits of an id's last character,
// which a lenient decoder maps back to the same bytes.
func flipTrailingBits(t *testing.T, id string) string {
	t.Helper()
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	require.NotZero(t, len(id)%4, "the id must end in a partial group to have trailing bits")
	last := strings.IndexByte(alphabet, id[len(id)-1])
	require.GreaterOrEqual(t, last, 0)
	return id[:len(id)-1] + string(alphabet[last^1])
}

func TestTaskSeal(t *testing.T) {
	const caller = "alice"
	// "job-7" makes the decoded id 47 bytes, so its last character carries
	// two unused bits.
	const handlerID = "job-7"
	a := testTaskSealer(1, "svc-a")

	tests := []struct {
		name   string
		taskID func(t *testing.T) string
		caller string
		want   string
		err    error
	}{
		{
			name:   "a round trip gives the handler id",
			taskID: func(t *testing.T) string { return mustSeal(t, a, handlerID, caller) },
			caller: caller,
			want:   handlerID,
		},
		{
			name:   "an empty handler id round-trips",
			taskID: func(t *testing.T) string { return mustSeal(t, a, "", "") },
			want:   "",
		},
		{
			name: "a flipped MAC byte is unknown",
			taskID: func(t *testing.T) string {
				raw := rawTaskID(t, mustSeal(t, a, handlerID, caller))
				raw[len(raw)-1] ^= 0x01
				return base64.RawURLEncoding.EncodeToString(raw)
			},
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name:   "another caller is unknown",
			taskID: func(t *testing.T) string { return mustSeal(t, a, handlerID, caller) },
			caller: "mallory",
			err:    errTaskUnknown,
		},
		{
			name:   "the unbound caller does not open a bound id",
			taskID: func(t *testing.T) string { return mustSeal(t, a, handlerID, caller) },
			caller: "",
			err:    errTaskUnknown,
		},
		{
			name:   "another epoch is gone",
			taskID: func(t *testing.T) string { return mustSeal(t, testTaskSealer(2, "svc-a"), handlerID, caller) },
			caller: caller,
			err:    errTaskGone,
		},
		{
			name: "another epoch stays gone with a garbage MAC",
			taskID: func(t *testing.T) string {
				raw := rawTaskID(t, mustSeal(t, testTaskSealer(2, "svc-a"), handlerID, caller))
				for i := len(raw) - taskIDMACLen; i < len(raw); i++ {
					raw[i] ^= 0xff
				}
				return base64.RawURLEncoding.EncodeToString(raw)
			},
			caller: caller,
			err:    errTaskGone,
		},
		{
			name:   "over 128 characters is unknown before decoding",
			taskID: func(*testing.T) string { return strings.Repeat("!", 200) },
			err:    errTaskUnknown,
		},
		{
			name: "over 128 characters is unknown even when authentic",
			taskID: func(t *testing.T) string {
				raw := rawTaskID(t, mustSeal(t, a, "", caller))
				body := append([]byte(nil), raw[:taskIDFixedLen-taskIDMACLen-1]...)
				long := strings.Repeat("x", 60)
				body = binary.AppendUvarint(body, uint64(len(long)))
				body = append(body, long...)
				body = append(body, make([]byte, taskIDMACLen)...)
				id := resealTaskID(a, body, caller)
				require.Greater(t, len(id), 128)
				return id
			},
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name:   "a trailing CRLF is unknown",
			taskID: func(t *testing.T) string { return mustSeal(t, a, handlerID, caller) + "\r\n" },
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name:   "altered trailing bits are unknown",
			taskID: func(t *testing.T) string { return flipTrailingBits(t, mustSeal(t, a, handlerID, caller)) },
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name:   "padding is unknown",
			taskID: func(t *testing.T) string { return mustSeal(t, a, handlerID, caller) + "=" },
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name: "an unknown version byte is unknown",
			taskID: func(t *testing.T) string {
				raw := rawTaskID(t, mustSeal(t, a, handlerID, caller))
				raw[0] = 2
				return resealTaskID(a, raw, caller)
			},
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name: "a handler id length past the MAC is unknown",
			taskID: func(t *testing.T) string {
				raw := rawTaskID(t, mustSeal(t, a, handlerID, caller))
				raw[taskIDFixedLen-taskIDMACLen-1]++
				return resealTaskID(a, raw, caller)
			},
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name:   "a truncated id is unknown",
			taskID: func(t *testing.T) string { return mustSeal(t, a, handlerID, caller)[:40] },
			caller: caller,
			err:    errTaskUnknown,
		},
		{
			name:   "an empty id is unknown",
			taskID: func(*testing.T) string { return "" },
			err:    errTaskUnknown,
		},
		{
			name:   "the same key with another namespace of the same length is unknown",
			taskID: func(t *testing.T) string { return mustSeal(t, testTaskSealer(1, "svc-b"), handlerID, caller) },
			caller: caller,
			err:    errTaskUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.open(tt.taskID(t), tt.caller)
			if tt.err != nil {
				require.ErrorIs(t, err, tt.err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("two seals of one handler id differ", func(t *testing.T) {
		assert.NotEqual(t, mustSeal(t, a, handlerID, caller), mustSeal(t, a, handlerID, caller))
	})

	t.Run("a 54-byte handler id fits in 128 characters", func(t *testing.T) {
		id := mustSeal(t, a, strings.Repeat("h", 54), caller)
		assert.LessOrEqual(t, len(id), 128)
		got, err := a.open(id, caller)
		require.NoError(t, err)
		assert.Equal(t, strings.Repeat("h", 54), got)
	})

	t.Run("a 55-byte handler id is too long", func(t *testing.T) {
		id, err := a.seal(strings.Repeat("h", 55), caller)
		require.ErrorIs(t, err, ErrTaskIDTooLong)
		assert.Empty(t, id)
	})

	t.Run("two servers with the same key open each other's ids", func(t *testing.T) {
		key := []byte(strings.Repeat("k", 32))
		one := NewMCPServer("svc", "1", WithTaskKey(key))
		two := NewMCPServer("svc", "2", WithTaskKey(key))
		id := mustSeal(t, one.taskSealer, handlerID, caller)
		got, err := two.taskSealer.open(id, caller)
		require.NoError(t, err)
		assert.Equal(t, handlerID, got)
	})

	t.Run("the key is copied when the option runs", func(t *testing.T) {
		key := []byte(strings.Repeat("k", 32))
		one := NewMCPServer("svc", "1", WithTaskKey(key))
		id := mustSeal(t, one.taskSealer, handlerID, caller)
		key[0] = 'x'
		got, err := one.taskSealer.open(id, caller)
		require.NoError(t, err)
		assert.Equal(t, handlerID, got)
	})

	t.Run("the namespace defaults to the server's name", func(t *testing.T) {
		key := []byte(strings.Repeat("k", 32))
		named := NewMCPServer("svc-a", "1", WithTaskKey(key))
		other := NewMCPServer("svc-b", "1", WithTaskKey(key))
		set := NewMCPServer("svc-b", "1", WithTaskKey(key), WithTaskNamespace("svc-a"))
		id := mustSeal(t, named.taskSealer, handlerID, caller)
		_, err := other.taskSealer.open(id, caller)
		require.ErrorIs(t, err, errTaskUnknown)
		got, err := set.taskSealer.open(id, caller)
		require.NoError(t, err)
		assert.Equal(t, handlerID, got)
	})

	t.Run("two default-key servers answer gone", func(t *testing.T) {
		one := NewMCPServer("svc", "1")
		two := NewMCPServer("svc", "1")
		id := mustSeal(t, one.taskSealer, handlerID, caller)
		_, err := two.taskSealer.open(id, caller)
		require.ErrorIs(t, err, errTaskGone)
		got, err := one.taskSealer.open(id, caller)
		require.NoError(t, err)
		assert.Equal(t, handlerID, got)
	})
}

// TestTaskSealKnownAnswer pins the bytes the id and its epoch are made of
// against a vector built outside this package: Python's hmac, hashlib and
// base64 over the spec's layout, with the nonce a0..af. Round trips cannot
// catch a MAC that binds the wrong bytes, since seal and open share the code.
func TestTaskSealKnownAnswer(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	wantEpoch := [taskIDEpochLen]byte{0x51, 0x95, 0x19, 0x45, 0x9f, 0xc6, 0xef, 0xba}
	wantTag := []byte{0xf5, 0x4a, 0xa6, 0x37, 0xa2, 0x8b, 0x1b, 0xb5, 0x6e, 0xcf, 0x2f, 0x6c, 0xf1, 0x29, 0xc6, 0xb0}
	const id = "AaChoqOkpaanqKmqq6ytrq9RlRlFn8bvughqb2ItMDAwN_VKpjeiixu1bs8vbPEpxrA"

	s := NewMCPServer("svc-a", "1", WithTaskKey(key)).taskSealer
	assert.Equal(t, wantEpoch, s.epoch, "the epoch derived from the key")

	raw := rawTaskID(t, id)
	mac := s.mac(raw[:len(raw)-taskIDMACLen], "alice")
	assert.Equal(t, wantTag, mac[:taskIDMACLen], "the MAC")

	got, err := s.open(id, "alice")
	require.NoError(t, err)
	assert.Equal(t, "job-0007", got)
}

func TestWithTaskKeyRefusesAShortKey(t *testing.T) {
	assert.Panics(t, func() { WithTaskKey(make([]byte, 31)) })
	assert.NotPanics(t, func() { WithTaskKey(make([]byte, 32)) })
}

func mustSeal(t *testing.T, s *taskSealer, handlerID, caller string) string {
	t.Helper()
	id, err := s.seal(handlerID, caller)
	require.NoError(t, err)
	return id
}
