package webauthn

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// A minimal CBOR decoder (RFC 8949): what an authenticator sends and
// nothing more. Definite lengths only, no tags, no floats: WebAuthn's
// attestation objects and COSE keys use neither, and refusing what is
// not expected is safer than half-understanding it. It exists because the
// core takes no dependencies.

// maxDepth and maxItems bound a hostile input: an attestation object is a
// few hundred bytes, and a claimed length of four billion must not
// allocate anything.
const (
	maxDepth = 16
	maxItems = 1 << 12
)

var errCBOR = errors.New("cbor: malformed")

// decode reads one item from b and returns it with the rest. Maps become
// map[any]any with int64 or string keys; byte strings []byte; text
// strings string; unsigned and negative integers int64; arrays []any.
func decode(b []byte) (any, []byte, error) { return decodeDepth(b, 0) }

func decodeDepth(b []byte, depth int) (any, []byte, error) {
	if depth > maxDepth || len(b) == 0 {
		return nil, nil, errCBOR
	}
	major, info := b[0]>>5, b[0]&0x1f
	b = b[1:]
	var n uint64
	switch {
	case info < 24:
		n = uint64(info)
	case info == 24 && len(b) >= 1:
		n, b = uint64(b[0]), b[1:]
	case info == 25 && len(b) >= 2:
		n, b = uint64(binary.BigEndian.Uint16(b)), b[2:]
	case info == 26 && len(b) >= 4:
		n, b = uint64(binary.BigEndian.Uint32(b)), b[4:]
	case info == 27 && len(b) >= 8:
		n, b = binary.BigEndian.Uint64(b), b[8:]
	default:
		return nil, nil, fmt.Errorf("%w: unsupported length encoding %d", errCBOR, info)
	}
	switch major {
	case 0:
		if n > 1<<62 {
			return nil, nil, errCBOR
		}
		return int64(n), b, nil
	case 1:
		if n > 1<<62 {
			return nil, nil, errCBOR
		}
		return -1 - int64(n), b, nil
	case 2, 3:
		if n > uint64(len(b)) {
			return nil, nil, errCBOR
		}
		if major == 2 {
			return append([]byte(nil), b[:n]...), b[n:], nil
		}
		return string(b[:n]), b[n:], nil
	case 4:
		if n > maxItems {
			return nil, nil, errCBOR
		}
		out := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			var v any
			var err error
			if v, b, err = decodeDepth(b, depth+1); err != nil {
				return nil, nil, err
			}
			out = append(out, v)
		}
		return out, b, nil
	case 5:
		if n > maxItems {
			return nil, nil, errCBOR
		}
		out := make(map[any]any, n)
		for i := uint64(0); i < n; i++ {
			var k, v any
			var err error
			if k, b, err = decodeDepth(b, depth+1); err != nil {
				return nil, nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, nil, fmt.Errorf("%w: map key of type %T", errCBOR, k)
			}
			if v, b, err = decodeDepth(b, depth+1); err != nil {
				return nil, nil, err
			}
			out[k] = v
		}
		return out, b, nil
	case 7:
		switch info {
		case 20:
			return false, b, nil
		case 21:
			return true, b, nil
		case 22, 23:
			return nil, b, nil
		}
		return nil, nil, fmt.Errorf("%w: simple value %d", errCBOR, info)
	}
	return nil, nil, fmt.Errorf("%w: major type %d", errCBOR, major)
}
