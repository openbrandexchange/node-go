// Package ids creates opaque, time-sortable object identifiers such as
// "prd_01j9zk3q7t5wabcdefghjkmnpq". They match the OBX Id pattern.
package ids

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz" // Crockford base32, lowercase

var (
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
)

// New returns prefix + "_" + a 26 character ULID-style string. Ids created
// in the same process are strictly increasing, so ordering by id equals
// creation order.
func New(prefix string) string {
	mu.Lock()
	ms := uint64(time.Now().UnixMilli())
	var r [10]byte
	if ms <= lastMS {
		ms = lastMS
		r = lastRand
		// increment the random part
		for i := len(r) - 1; i >= 0; i-- {
			r[i]++
			if r[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(r[:]); err != nil {
			panic(err)
		}
	}
	lastMS, lastRand = ms, r
	mu.Unlock()

	var b [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ms)
	copy(b[:6], ts[2:])
	copy(b[6:], r[:])
	return prefix + "_" + encode(b)
}

func encode(b [16]byte) string {
	// 128 bits -> 26 base32 characters (first character carries 3 bits).
	out := make([]byte, 26)
	var acc uint64
	var bits uint
	idx := 25
	for i := 15; i >= 0; i-- {
		acc |= uint64(b[i]) << bits
		bits += 8
		for bits >= 5 && idx >= 0 {
			out[idx] = alphabet[acc&31]
			acc >>= 5
			bits -= 5
			idx--
		}
	}
	for idx >= 0 {
		out[idx] = alphabet[acc&31]
		acc >>= 5
		idx--
	}
	return string(out)
}
