package lookup

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// Predeclared protocol (2026-09-17), written before running benchmarks.
// Earlier topic 18 had substantial noise; this is a new experiment, not a
// retest of it. No timing predictions are inferred from the source application.
//
// H1: For 128 entries, eight requested fields at the tail require 996 entry
//     visits in repeated linear search, versus 128 in one scan. With all fields
//     absent the counts are 1024 and 128. A first-field-at-front lookup visits
//     one entry in either algorithm. TestVerify_Visits checks these formulas.
// H2: SliceRepeated, SliceScan, MapRepeated and MapScan allocate zero bytes per
//     extraction; IndexEach allocates for its new map. A measured allocation in
//     a direct/scan path or zero allocations in IndexEach refutes this claim.
// H3: Native map lookup performs k keyed queries; iterating that map can inspect
//     up to n entries. At n=128,k=1, MapScan is expected to be slower than
//     MapRepeated beyond same-row noise and uncertainty. Otherwise timing is
//     inconclusive, even though the algorithmic distinction remains.
// H4: Eight fields at the tail of 128 entries should favor SliceScan over
//     SliceRepeated beyond same-row noise and uncertainty. One field at the
//     front has no visit-count saving; no speedup is promised for that case.
// H5: Per-record index construction is paid once in BatchIndexed, so B/op and
//     allocs/op stay constant as rounds grows 1 -> 8 -> 32. BatchExtracted and
//     BatchRepeated stay at zero allocation. Reusing a fixed-field Result should
//     avoid later searches, but does not support arbitrary new query fields.
//
// One complete batch: -cpu=1 -count=8 -benchtime=200ms, no selective reruns or
// sample exclusions. Report every median and benchstat 95% interval. For a
// timing claim require a gap beyond its matched duplicate-function noise AND
// non-overlapping intervals; otherwise report inconclusive. No global speedup.
// Use disjoint full sample ranges as a conservative sufficient condition for
// non-overlapping intervals; do not infer acceptance from rounded percentages.
// Extraction: n=16/128, k=1/8, front/tail/shuffled/absent, seven variants.
// Shuffled uses a deterministic seed (7); map hash seeds/iteration remain random.
// Batch reuse: n=128,k=8, front/tail, rounds=1/8/32, four variants.
// One extraction op handles one record. One batch op handles one record with
// rounds identical consumers; building an index or extracting a Result is timed.
// Synthetic string-only values; first duplicate wins; empty != absent.
// noinline preserves callable work; common result-copy/call overhead is included.
// Source generation and initial native-map conversion are outside b.Loop.

var resultSink Result
var sumSink int

func fixture(n int, layout string) []Attribute {
	attrs := make([]Attribute, 0, n)
	if layout != "absent" {
		for _, key := range queryKeys {
			attrs = append(attrs, Attribute{strings.Clone(key), "value:" + key})
		}
	}
	for i := len(attrs); i < n; i++ {
		attrs = append(attrs, Attribute{fmt.Sprintf("extra.%08x", i), "unused"})
	}
	switch layout {
	case "front", "absent":
	case "tail":
		attrs = append(append([]Attribute{}, attrs[8:]...), attrs[:8]...)
	case "shuffled":
		rng := rand.New(rand.NewPCG(7, 19))
		rng.Shuffle(len(attrs), func(i, j int) { attrs[i], attrs[j] = attrs[j], attrs[i] })
	default:
		panic("unknown layout")
	}
	return attrs
}

// oracle deliberately does not use slot, get or BuildIndex.
func oracle(attrs []Attribute, k int) Result {
	var out Result
	for i, key := range queryKeys[:k] {
		for j := 0; j < len(attrs); j++ {
			if attrs[j].Key == key {
				out.Values[i], out.Present[i] = attrs[j].Value, true
				break
			}
		}
	}
	return out
}

func verify(t *testing.T, attrs []Attribute) {
	t.Helper()
	before := append([]Attribute(nil), attrs...)
	index := BuildIndex(attrs)
	for k := 0; k <= len(queryKeys); k++ {
		want := oracle(attrs, k)
		for name, got := range map[string]Result{
			"SliceRepeated": SliceRepeated(attrs, k), "SliceRepeatedNoise": SliceRepeatedNoise(attrs, k),
			"SliceScan": SliceScan(attrs, k), "IndexEach": IndexEach(attrs, k),
			"MapRepeated": MapRepeated(index, k), "MapRepeatedNoise": MapRepeatedNoise(index, k),
			"MapScan": MapScan(index, k),
		} {
			if got != want {
				t.Fatalf("%s k=%d: got %+v, want %+v", name, k, got, want)
			}
		}
	}
	for i := range attrs {
		if attrs[i] != before[i] {
			t.Fatal("source mutated")
		}
	}
}

func TestExtraction(t *testing.T) {
	verify(t, nil)
	verify(t, []Attribute{{"method", ""}, {"method", "later"}, {"service", "first"}, {"service", "later"}, {"extra", "x"}})
	for _, n := range []int{8, 16, 31, 32, 128} {
		for _, layout := range []string{"front", "tail", "shuffled", "absent"} {
			verify(t, fixture(n, layout))
		}
	}
	if SliceScan([]Attribute{{"method", ""}}, 1) == SliceScan(nil, 1) {
		t.Fatal("empty collapsed into missing")
	}
	if MapRepeated(nil, 8) != (Result{}) || MapScan(nil, 8) != (Result{}) {
		t.Fatal("nil map")
	}
}

func TestSnapshot(t *testing.T) {
	attrs := []Attribute{{"method", "GET"}}
	index, extracted := BuildIndex(attrs), SliceScan(attrs, 1)
	attrs[0].Value = "POST"
	if index["method"] != "GET" || extracted.Values[0] != "GET" {
		t.Fatal("snapshot changed")
	}
	index["method"] = "DELETE"
	if attrs[0].Value != "POST" {
		t.Fatal("map mutation changed source")
	}
}

func TestSlot(t *testing.T) {
	for i, key := range queryKeys {
		if slot(key) != i {
			t.Fatalf("slot %s", key)
		}
	}
	for _, key := range []string{"", "Method", "extra", "method\x00", "方法"} {
		if slot(key) != -1 {
			t.Fatalf("unknown slot %q", key)
		}
	}
}

func TestBatch(t *testing.T) {
	for _, layout := range []string{"front", "tail", "shuffled", "absent"} {
		attrs := fixture(128, layout)
		for _, k := range []int{0, 1, 8} {
			for _, rounds := range []int{0, 1, 8, 32} {
				want := consume(oracle(attrs, k)) * rounds
				if BatchRepeated(attrs, k, rounds) != want || BatchRepeatedNoise(attrs, k, rounds) != want ||
					BatchIndexed(attrs, k, rounds) != want || BatchExtracted(attrs, k, rounds) != want {
					t.Fatal("batch mismatch")
				}
			}
		}
	}
}

func FuzzExtraction(f *testing.F) {
	f.Add([]byte{0, 1, 0, 7, 9})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 128 {
			data = data[:128]
		}
		attrs := make([]Attribute, len(data))
		for i, b := range data {
			key := "extra"
			if int(b)%10 < len(queryKeys) {
				key = queryKeys[int(b)%10]
			}
			value := ""
			if b&1 != 0 {
				value = fmt.Sprint(i)
			}
			attrs[i] = Attribute{key, value}
		}
		verify(t, attrs)
	})
}

// Diagnostic counting models the two loops separately from timed code. It
// counts attribute visits, not machine instructions or switch comparisons.
func visits(attrs []Attribute, k int) (repeated, scanned int) {
	for _, key := range queryKeys[:k] {
		for _, attr := range attrs {
			repeated++
			if attr.Key == key {
				break
			}
		}
	}
	var found [8]bool
	remaining := k
	if remaining == 0 {
		return
	}
	for _, attr := range attrs {
		scanned++
		i := slot(attr.Key)
		if i >= 0 && i < k && !found[i] {
			found[i] = true
			remaining--
		}
		if remaining == 0 {
			break
		}
	}
	return
}

func TestVerify_Visits(t *testing.T) {
	for _, tc := range []struct {
		layout               string
		k, repeated, scanned int
	}{
		{"tail", 8, 996, 128}, {"absent", 8, 1024, 128}, {"front", 8, 36, 8},
		{"front", 1, 1, 1}, {"tail", 1, 121, 121}, {"absent", 1, 128, 128},
	} {
		repeated, scanned := visits(fixture(128, tc.layout), tc.k)
		if repeated != tc.repeated || scanned != tc.scanned {
			t.Fatalf("%+v got %d/%d", tc, repeated, scanned)
		}
		t.Logf("n=128 k=%d layout=%s: repeated=%d scan=%d visits", tc.k, tc.layout, repeated, scanned)
	}
}

func BenchmarkExtract(b *testing.B) {
	for _, n := range []int{16, 128} {
		for _, k := range []int{1, 8} {
			for _, layout := range []string{"front", "tail", "shuffled", "absent"} {
				attrs := fixture(n, layout)
				index := BuildIndex(attrs)
				variants := []struct {
					name    string
					extract func() Result
				}{
					{"SliceRepeated", func() Result { return SliceRepeated(attrs, k) }},
					{"SliceRepeatedNoise", func() Result { return SliceRepeatedNoise(attrs, k) }},
					{"SliceScan", func() Result { return SliceScan(attrs, k) }},
					{"IndexEach", func() Result { return IndexEach(attrs, k) }},
					{"MapRepeated", func() Result { return MapRepeated(index, k) }},
					{"MapRepeatedNoise", func() Result { return MapRepeatedNoise(index, k) }},
					{"MapScan", func() Result { return MapScan(index, k) }},
				}
				for _, v := range variants {
					b.Run(fmt.Sprintf("n=%d/k=%d/layout=%s/%s", n, k, layout, v.name), func(b *testing.B) {
						b.ReportAllocs()
						var result Result
						for b.Loop() {
							result = v.extract()
						}
						resultSink = result
					})
				}
			}
		}
	}
}

func BenchmarkReuse(b *testing.B) {
	for _, layout := range []string{"front", "tail"} {
		attrs := fixture(128, layout)
		for _, rounds := range []int{1, 8, 32} {
			for _, v := range []struct {
				name string
				run  func([]Attribute, int, int) int
			}{
				{"Repeated", BatchRepeated}, {"RepeatedNoise", BatchRepeatedNoise},
				{"Indexed", BatchIndexed}, {"Extracted", BatchExtracted},
			} {
				b.Run(fmt.Sprintf("layout=%s/rounds=%d/%s", layout, rounds, v.name), func(b *testing.B) {
					b.ReportAllocs()
					var sum int
					for b.Loop() {
						sum = v.run(attrs, 8, rounds)
					}
					sumSink = sum
				})
			}
		}
	}
}
