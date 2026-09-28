// Package lookup compares repeated lookup, one-pass extraction and indexing.
// The schema is fixed to eight string fields; k selects its first k fields.
package lookup

var queryKeys = [...]string{"method", "route", "status", "service", "region", "tenant", "trace", "user"}

type Attribute struct {
	Key, Value string
}

// Result preserves empty values separately from missing fields. Strings retain
// their immutable backing storage; extraction does not deep-copy string bytes.
type Result struct {
	Values  [8]string
	Present [8]bool
}

func slot(key string) int {
	switch key {
	case "method":
		return 0
	case "route":
		return 1
	case "status":
		return 2
	case "service":
		return 3
	case "region":
		return 4
	case "tenant":
		return 5
	case "trace":
		return 6
	case "user":
		return 7
	default:
		return -1
	}
}

func get(attrs []Attribute, key string) (string, bool) {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return "", false
}

// SliceRepeated searches from the start for each field. First duplicate wins.
// All extraction functions require 0 <= k <= 8, enforced by array slicing.
//
//go:noinline
func SliceRepeated(attrs []Attribute, k int) Result {
	var out Result
	for i, key := range queryKeys[:k] {
		out.Values[i], out.Present[i] = get(attrs, key)
	}
	return out
}

//go:noinline
func SliceRepeatedNoise(attrs []Attribute, k int) Result {
	var out Result
	for i, key := range queryKeys[:k] {
		out.Values[i], out.Present[i] = get(attrs, key)
	}
	return out
}

// SliceScan dispatches keys to fixed result slots and stops when all requested
// fields are found. A switch replaces the repeated linear searches; scanning
// with a second loop over queryKeys would still perform O(n*k) comparisons.
//
//go:noinline
func SliceScan(attrs []Attribute, k int) Result {
	var out Result
	remaining := len(queryKeys[:k])
	if remaining == 0 {
		return out
	}
	for _, attr := range attrs {
		i := slot(attr.Key)
		if i < 0 || i >= k || out.Present[i] {
			continue
		}
		out.Values[i], out.Present[i] = attr.Value, true
		remaining--
		if remaining == 0 {
			break
		}
	}
	return out
}

// BuildIndex processes entries in reverse so first-duplicate-wins matches get.
// An index is a snapshot: changes to the source slice do not update this map.
//
//go:noinline
func BuildIndex(attrs []Attribute) map[string]string {
	index := make(map[string]string, len(attrs))
	for i := len(attrs) - 1; i >= 0; i-- {
		index[attrs[i].Key] = attrs[i].Value
	}
	return index
}

//go:noinline
func MapRepeated(attrs map[string]string, k int) Result {
	var out Result
	for i, key := range queryKeys[:k] {
		out.Values[i], out.Present[i] = attrs[key]
	}
	return out
}

//go:noinline
func MapRepeatedNoise(attrs map[string]string, k int) Result {
	var out Result
	for i, key := range queryKeys[:k] {
		out.Values[i], out.Present[i] = attrs[key]
	}
	return out
}

// MapScan has the same fixed-schema dispatch as SliceScan. Map iteration order
// is unspecified, so source-slice position does not determine its early exit.
//
//go:noinline
func MapScan(attrs map[string]string, k int) Result {
	var out Result
	remaining := len(queryKeys[:k])
	if remaining == 0 {
		return out
	}
	for key, value := range attrs {
		i := slot(key)
		if i < 0 || i >= k {
			continue
		}
		out.Values[i], out.Present[i] = value, true
		remaining--
		if remaining == 0 {
			break
		}
	}
	return out
}

// IndexEach includes building the entire index in each extraction operation.
//
//go:noinline
func IndexEach(attrs []Attribute, k int) Result {
	return MapRepeated(BuildIndex(attrs), k)
}

// consume makes each batch consumer observable. It represents identical cheap
// downstream work, not a real application. Its cost is included in every batch.
//
//go:noinline
func consume(result Result) int {
	sum := 0
	for i, present := range result.Present {
		if present {
			sum += len(result.Values[i]) + 1
		}
	}
	return sum
}

// Each batch models one immutable record read repeatedly for the SAME fields.
// rounds must be nonnegative. Index/extraction preparation is timed once per
// batch, making its amortization visible without a free prebuilt index.
//
//go:noinline
func BatchRepeated(attrs []Attribute, k, rounds int) int {
	sum := 0
	for range rounds {
		sum += consume(SliceRepeated(attrs, k))
	}
	return sum
}

//go:noinline
func BatchRepeatedNoise(attrs []Attribute, k, rounds int) int {
	sum := 0
	for range rounds {
		sum += consume(SliceRepeated(attrs, k))
	}
	return sum
}

//go:noinline
func BatchIndexed(attrs []Attribute, k, rounds int) int {
	index := BuildIndex(attrs)
	sum := 0
	for range rounds {
		sum += consume(MapRepeated(index, k))
	}
	return sum
}

//go:noinline
func BatchExtracted(attrs []Attribute, k, rounds int) int {
	result := SliceScan(attrs, k)
	sum := 0
	for range rounds {
		sum += consume(result)
	}
	return sum
}
