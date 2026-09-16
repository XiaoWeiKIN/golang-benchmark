package bitmap

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// Predeclared hypotheses, before the first benchmark run (2026-09-16).
// H1: ByteSet is exactly 32 B and matches map membership for all 256 values.
//     Steady-state lookups allocate zero bytes for bitmap, bool array and map.
// H2: String filters have no false negatives and all lookup variants allocate
//     zero bytes. PrefixMiss reaches the exact map 0% of the time; LengthMiss
//     reaches it 100% with Prefix but 0% with Shape. Collision and Hit reach it
//     100% with both. TestVerify_CandidateRates checks these independently.
// H3: On 256 rules, PrefixMiss and LengthMiss, eliminating exact lookups reduces
//     ns/op beyond the same-row Direct/DirectNoise difference. If not, evidence
//     for the timing benefit is inconclusive; the lookup count claim survives.
// H4: When both filters admit everything (Hit, Collision), filtering cannot
//     save any exact lookup; any speedup needs evidence beyond this mechanism.
//     We expect at least one regression above the noise floor, but do not make
//     correctness or acceptance conditional on finding a regression.
// H5: Construction is not free: a StringSet embeds exactly 8208 bytes of feature
//     bits, in addition to a map. Measure map-only and filtered construction
//     separately; no build work is charged to steady-state lookup benchmarks.
//
// Protocol: one batch, -cpu=1 -count=8 -benchtime=100ms, all groups including
// per-input DirectNoise. Report medians and benchstat uncertainty. No selective
// reruns or exclusions. Insufficient timing resolution is reported as such.
// Each lookup op consumes one key from a fixed shuffled corpus of 1000 keys.
// Rules: 8/256, byte lengths: 16/64, two corpus seeds. Inputs are synthetic;
// Hits clone the key bytes so pointer identity is not the intended shortcut.
// Construction and input generation are outside the lookup timer (b.Loop).

var lookupSink bool
var mapSink map[string]struct{}
var setSink *StringSet

func ruleKeys(n, length int) []string {
	prefixes := []string{"ht", "db", "rp", "ne", "me", "se", "pr", "cu"}
	keys := make([]string, n)
	for i := range keys {
		key := fmt.Sprintf("%s.%08x.", prefixes[i%len(prefixes)], i)
		keys[i] = key + strings.Repeat("x", length-len(key))
	}
	return keys
}

func corpus(rules []string, kind string, seed uint64) []string {
	keys := make([]string, 1000)
	for i := range keys {
		key := rules[i%len(rules)]
		switch kind {
		case "PrefixMiss":
			key = "zz" + key[2:]
		case "LengthMiss":
			key += "!"
		case "Collision":
			key = key[:len(key)-1] + "!"
		case "Hit":
			key = strings.Clone(key)
		case "Mixed10":
			if i%10 == 0 {
				key = strings.Clone(key)
			} else if i%2 == 0 {
				key += "!"
			} else {
				key = "zz" + key[2:]
			}
		default:
			panic("unknown corpus kind")
		}
		keys[i] = key
	}
	rng := rand.New(rand.NewPCG(seed, 42))
	rng.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	return keys
}

func TestByteSet(t *testing.T) {
	var set ByteSet
	if unsafe.Sizeof(set) != 32 {
		t.Fatal("bitmap is not 32 bytes")
	}
	for i := 0; i < 256; i += 3 {
		set.Add(byte(i))
	}
	for i := 0; i < 256; i++ {
		if set.Contains(byte(i)) != (i%3 == 0) {
			t.Fatalf("wrong membership for %d", i)
		}
	}
	set.Add(255)
	set.Add(255)
	if !set.Contains(255) {
		t.Fatal("last bit missing")
	}
}

func assertEquivalent(t *testing.T, keys, probes []string) {
	t.Helper()
	set := NewStringSet(keys)
	want := make(map[string]bool, len(keys))
	for _, key := range keys {
		want[key] = true
	}
	for _, key := range probes {
		if want[key] && (!set.MayContainPrefix(key) || !set.MayContainShape(key)) {
			t.Fatalf("false negative: %q", key)
		}
		if set.Direct(key) != want[key] || set.DirectNoise(key) != want[key] ||
			set.Prefix(key) != want[key] || set.Shape(key) != want[key] {
			t.Fatalf("incorrect lookup: %q", key)
		}
	}
}

func TestStringSet(t *testing.T) {
	keys := []string{"", "a", "a\x00", "\x00\x00", "\xff\xff", "字段", "db.x", "http.code"}
	for _, n := range []int{63, 64, 127, 128, 129, 255, 256, 1024} {
		keys = append(keys, strings.Repeat("x", n))
	}
	assertEquivalent(t, keys, append(append([]string{}, keys...), "absent", "db.y", "http.none"))
	assertEquivalent(t, nil, keys)
	assertEquivalent(t, []string{"a", "a"}, []string{"a", "", "a\x00"})
	// All two-byte prefixes, including the highest bitmap word and bit.
	all := make([]string, 1<<16)
	for i := range all {
		all[i] = string([]byte{byte(i >> 8), byte(i)})
	}
	assertEquivalent(t, all, all)
}

func TestFalsePositives(t *testing.T) {
	// Independent feature sets admit an unseen combination, and lengths alias
	// modulo 128. Neither candidate is a member until the exact map says so.
	set := NewStringSet([]string{"aa12", "bb12345"})
	for _, key := range []string{"aa12345", "aa" + strings.Repeat("x", 130)} {
		if !set.MayContainShape(key) || set.Shape(key) {
			t.Fatalf("expected safe false positive: %q", key)
		}
	}
}

func TestVerify_CandidateRates(t *testing.T) {
	for _, n := range []int{8, 256} {
		for _, length := range []int{16, 64} {
			rules := ruleKeys(n, length)
			set := NewStringSet(rules)
			for _, seed := range []uint64{7, 91} {
				for _, kind := range []string{"PrefixMiss", "LengthMiss", "Collision", "Hit", "Mixed10"} {
					keys := corpus(rules, kind, seed)
					var got [3]int
					for _, key := range keys {
						if set.MayContainPrefix(key) {
							got[0]++
						}
						if set.MayContainShape(key) {
							got[1]++
						}
						if set.Direct(key) {
							got[2]++
						}
					}
					want := map[string][3]int{
						"PrefixMiss": {0, 0, 0}, "LengthMiss": {1000, 0, 0},
						"Collision": {1000, 1000, 0}, "Hit": {1000, 1000, 1000},
						"Mixed10": {500, 100, 100},
					}[kind]
					if got != want {
						t.Fatalf("%s: %v != %v", kind, got, want)
					}
					assertEquivalent(t, rules, keys)
				}
			}
		}
	}
}

func FuzzStringSet(f *testing.F) {
	f.Add("http.code", "db.operation", "http.none")
	f.Add("", "a\x00", "a")
	f.Fuzz(func(t *testing.T, a, b, probe string) {
		assertEquivalent(t, []string{a, b}, []string{a, b, probe})
	})
}

func BenchmarkByteMembership(b *testing.B) {
	var bits ByteSet
	var flags ByteFlags
	members := make(ByteMap)
	for i := 0; i < 256; i += 3 {
		bits.Add(byte(i))
		flags[i] = true
		members[byte(i)] = struct{}{}
	}
	for _, variant := range []struct {
		name   string
		lookup func(byte) bool
	}{
		{"Bitmap", bits.Contains},
		{"BoolArray", flags.Contains},
		{"Map", members.Contains},
		{"MapNoise", members.ContainsNoise},
	} {
		b.Run(variant.name, func(b *testing.B) {
			b.ReportAllocs()
			var i byte
			var result bool
			for b.Loop() {
				result = variant.lookup(i)
				i++
			}
			lookupSink = result
		})
	}
}

func BenchmarkLookup(b *testing.B) {
	for _, n := range []int{8, 256} {
		for _, length := range []int{16, 64} {
			rules := ruleKeys(n, length)
			set := NewStringSet(rules)
			for _, seed := range []uint64{7, 91} {
				for _, kind := range []string{"PrefixMiss", "LengthMiss", "Collision", "Hit", "Mixed10"} {
					keys := corpus(rules, kind, seed)
					for _, variant := range []struct {
						name   string
						lookup func(string) bool
					}{
						{"Direct", set.Direct}, {"DirectNoise", set.DirectNoise},
						{"Prefix", set.Prefix}, {"Shape", set.Shape},
					} {
						b.Run(fmt.Sprintf("%s/rules=%d/len=%d/seed=%d/%s", kind, n, length, seed, variant.name), func(b *testing.B) {
							b.ReportAllocs()
							i := 0
							var result bool
							for b.Loop() {
								result = variant.lookup(keys[i])
								i++
								if i == len(keys) {
									i = 0
								}
							}
							lookupSink = result
						})
					}
				}
			}
		}
	}
}

func BenchmarkBuild(b *testing.B) {
	for _, n := range []int{8, 256} {
		keys := ruleKeys(n, 16)
		b.Run(fmt.Sprintf("rules=%d/Map", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				m := make(map[string]struct{}, len(keys))
				for _, key := range keys {
					m[key] = struct{}{}
				}
				mapSink = m
			}
		})
		b.Run(fmt.Sprintf("rules=%d/Filtered", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				setSink = NewStringSet(keys)
			}
		})
	}
}

func TestLayout(t *testing.T) {
	var s StringSet
	if unsafe.Sizeof(s.prefixes)+unsafe.Sizeof(s.lengths) != 8208 {
		t.Fatal("unexpected feature size")
	}
	t.Logf("ByteSet=%d B, bool array=%d B, StringSet=%d B (map storage excluded)",
		unsafe.Sizeof(ByteSet{}), unsafe.Sizeof([256]bool{}), reflect.TypeOf(s).Size())
}
