package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
)

// ErrTaskIDTooLong is returned when a handler's task id is longer than the 54
// bytes a Tasks extension task id, at most 128 characters, has room for.
var ErrTaskIDTooLong = errors.New("the handler's task id is too long: a sealed task id has room for 54 bytes of it")

var (
	errTaskUnknown = errors.New("unknown: no such task")
	errTaskGone    = errors.New("gone: the task ended or the server restarted; call the tool again")
)

// The layout of a sealed task id, before its base64url encoding:
//
//	version(1) | nonce(16) | epoch(8) | uvarint(len(handlerID)) | handlerID | mac(16)
//
// Everything before the MAC is authenticated. The MAC is
//
//	HMAC-SHA256(key, taskIDMACDomain | uvarint(len(namespace)) | namespace |
//	            authenticated | uvarint(len(caller)) | caller)[:16]
//
// Every variable-length input carries its length, so no two inputs
// concatenate alike. The caller is authenticated but not carried.
const (
	taskIDVersion   = 1
	taskIDNonceLen  = 16
	taskIDEpochLen  = 8
	taskIDMACLen    = 16
	taskIDEpochOff  = 1 + taskIDNonceLen
	taskIDLenOff    = taskIDEpochOff + taskIDEpochLen
	taskIDFixedLen  = taskIDLenOff + 1 + taskIDMACLen
	taskIDMaxLen    = 128
	taskIDMaxRawLen = taskIDMaxLen / 4 * 3
	// A handler id up to this long has a one-byte uvarint length.
	taskIDMaxHandlerLen = taskIDMaxRawLen - taskIDFixedLen
	taskKeyMinLen       = 32
	taskIDMACDomain     = "flexmcp/tasks/id/v1"
	taskEpochDomain     = "flexmcp/tasks/epoch"
)

// taskSealer mints and checks Tasks extension task ids, so the server stores
// none. It is immutable once built, so safe for concurrent use.
type taskSealer struct {
	key       []byte
	epoch     [taskIDEpochLen]byte
	namespace string
}

// newTaskSealer builds the sealer for key, deriving its epoch, or for a
// random key and a random epoch when key is nil.
func newTaskSealer(key []byte, namespace string) *taskSealer {
	t := &taskSealer{key: key, namespace: namespace}
	if key == nil {
		t.key = make([]byte, taskKeyMinLen)
		// crypto/rand.Read never returns an error; it crashes instead.
		_, _ = rand.Read(t.key)
		_, _ = rand.Read(t.epoch[:])
		return t
	}
	h := hmac.New(sha256.New, key)
	_, _ = io.WriteString(h, taskEpochDomain)
	copy(t.epoch[:], h.Sum(nil))
	return t
}

// seal mints a task id for handlerID, bound to caller, under a fresh nonce.
// A handler id over taskIDMaxHandlerLen bytes returns ErrTaskIDTooLong.
func (t *taskSealer) seal(handlerID, caller string) (string, error) {
	if len(handlerID) > taskIDMaxHandlerLen {
		return "", ErrTaskIDTooLong
	}
	var raw [taskIDMaxRawLen]byte
	raw[0] = taskIDVersion
	_, _ = rand.Read(raw[1:taskIDEpochOff])
	copy(raw[taskIDEpochOff:taskIDLenOff], t.epoch[:])
	n := taskIDLenOff + binary.PutUvarint(raw[taskIDLenOff:], uint64(len(handlerID)))
	n += copy(raw[n:], handlerID)
	mac := t.mac(raw[:n], caller)
	n += copy(raw[n:], mac[:taskIDMACLen])
	return base64.RawURLEncoding.EncodeToString(raw[:n]), nil
}

// open checks taskID for caller and returns the handler id sealed in it. The
// checks run in this order, and the first to fail answers:
//
//  1. over 128 characters, undecodable, not spelled exactly as minted, or
//     of a version this server does not know: errTaskUnknown;
//  2. an epoch that is not this server's: errTaskGone, with the MAC
//     unchecked, so forging an old epoch gains nothing but "gone";
//  3. a MAC that fails for caller, compared in constant time: errTaskUnknown.
func (t *taskSealer) open(taskID, caller string) (handlerID string, err error) {
	// The length is checked before decoding, which then fits raw.
	if len(taskID) > taskIDMaxLen {
		return "", errTaskUnknown
	}
	var raw [taskIDMaxRawLen]byte
	n, err := base64.RawURLEncoding.Decode(raw[:], []byte(taskID))
	if err != nil || n < taskIDFixedLen {
		return "", errTaskUnknown
	}
	// The decoder skips CR and LF and ignores the unused bits of the last
	// character, so one id has many spellings. Only the one minted passes.
	var canonical [taskIDMaxLen]byte
	c := base64.RawURLEncoding.EncodedLen(n)
	base64.RawURLEncoding.Encode(canonical[:c], raw[:n])
	if string(canonical[:c]) != taskID {
		return "", errTaskUnknown
	}
	if raw[0] != taskIDVersion {
		return "", errTaskUnknown
	}
	end := n - taskIDMACLen
	size, k := binary.Uvarint(raw[taskIDLenOff:end])
	if k <= 0 || size != uint64(end-taskIDLenOff-k) {
		return "", errTaskUnknown
	}
	if [taskIDEpochLen]byte(raw[taskIDEpochOff:taskIDLenOff]) != t.epoch {
		return "", errTaskGone
	}
	mac := t.mac(raw[:end], caller)
	if !hmac.Equal(mac[:taskIDMACLen], raw[end:n]) {
		return "", errTaskUnknown
	}
	return string(raw[taskIDLenOff+k : end]), nil
}

// mac returns the whole HMAC-SHA256 of the layout above; an id carries its
// first taskIDMACLen bytes.
func (t *taskSealer) mac(authenticated []byte, caller string) [sha256.Size]byte {
	h := hmac.New(sha256.New, t.key)
	var size [binary.MaxVarintLen64]byte
	_, _ = io.WriteString(h, taskIDMACDomain)
	_, _ = h.Write(size[:binary.PutUvarint(size[:], uint64(len(t.namespace)))])
	_, _ = io.WriteString(h, t.namespace)
	_, _ = h.Write(authenticated)
	_, _ = h.Write(size[:binary.PutUvarint(size[:], uint64(len(caller)))])
	_, _ = io.WriteString(h, caller)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}
