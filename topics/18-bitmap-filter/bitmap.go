// Package bitmap demonstrates exact membership and conservative string filters.
package bitmap

// ByteSet is an exact bitmap for the entire byte domain: 256 bits, 32 bytes.
// Build it before publishing to readers; Add must not race with Contains.
type ByteSet [4]uint64

func (s *ByteSet) Add(value byte) { s[value>>6] |= uint64(1) << (value & 63) }

//go:noinline
func (s *ByteSet) Contains(value byte) bool {
	return s[value>>6]&(uint64(1)<<(value&63)) != 0
}

// ByteFlags provides the same exact membership with one bool per value.
type ByteFlags [256]bool

//go:noinline
func (s *ByteFlags) Contains(value byte) bool { return s[value] }

type ByteMap map[byte]struct{}

//go:noinline
func (s ByteMap) Contains(value byte) bool {
	_, ok := s[value]
	return ok
}

//go:noinline
func (s ByteMap) ContainsNoise(value byte) bool {
	_, ok := s[value]
	return ok
}

// StringSet is immutable after construction. The map remains the source of
// truth; feature bitmaps only reject impossible matches.
// Prefix and length are indexed independently, so their cross-product can admit
// candidates whose particular prefix/length combination was never registered.
type StringSet struct {
	exact    map[string]struct{}
	prefixes [1024]uint64 // All 65,536 two-byte prefixes: 8 KiB.
	lengths  [2]uint64    // Length modulo 128: 16 bytes, with safe collisions.
}

func NewStringSet(keys []string) *StringSet {
	s := &StringSet{exact: make(map[string]struct{}, len(keys))}
	for _, key := range keys {
		s.exact[key] = struct{}{}
		prefix := prefixOf(key)
		s.prefixes[prefix>>6] |= uint64(1) << (prefix & 63)
		n := uint(len(key)) & 127
		s.lengths[n>>6] |= uint64(1) << (n & 63)
	}
	return s
}

// Short strings use zero padding; e.g. "a" and "a\x00" have the same prefix.
// This is safe: collisions only admit extra candidates, never exclude a member.
func prefixOf(key string) uint16 {
	if len(key) == 0 {
		return 0
	}
	prefix := uint16(key[0]) << 8
	if len(key) > 1 {
		prefix |= uint16(key[1])
	}
	return prefix
}

func (s *StringSet) MayContainPrefix(key string) bool {
	prefix := prefixOf(key)
	return s.prefixes[prefix>>6]&(uint64(1)<<(prefix&63)) != 0
}

func (s *StringSet) MayContainShape(key string) bool {
	if !s.MayContainPrefix(key) {
		return false
	}
	n := uint(len(key)) & 127
	return s.lengths[n>>6]&(uint64(1)<<(n&63)) != 0
}

//go:noinline
func (s *StringSet) Direct(key string) bool {
	_, ok := s.exact[key]
	return ok
}

// DirectNoise deliberately duplicates Direct's body for a noise-floor control.
//
//go:noinline
func (s *StringSet) DirectNoise(key string) bool {
	_, ok := s.exact[key]
	return ok
}

//go:noinline
func (s *StringSet) Prefix(key string) bool {
	if !s.MayContainPrefix(key) {
		return false
	}
	_, ok := s.exact[key]
	return ok
}

//go:noinline
func (s *StringSet) Shape(key string) bool {
	if !s.MayContainShape(key) {
		return false
	}
	_, ok := s.exact[key]
	return ok
}
