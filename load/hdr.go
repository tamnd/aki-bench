package load

import (
	"fmt"
	"io"
	"math"
)

// Histogram is a high-dynamic-range latency histogram in the HdrHistogram style.
// It records values in a fixed range with a fixed number of significant figures
// at constant relative error, so a single structure covers nanoseconds to tens
// of seconds without losing resolution at the low end.
// Values are nanoseconds in this harness but the type is unit-agnostic.
// This is a clean port of the helper that ships inside aki's own bench package,
// kept here so aki-bench stays zero-dependency.
type Histogram struct {
	lowest                      int64
	highest                     int64
	sigFigs                     int
	unitMagnitude               uint32
	subBucketHalfCount          int32
	subBucketHalfCountMagnitude uint32
	subBucketCount              int32
	subBucketMask               int64
	bucketCount                 int32
	counts                      []int64
	totalCount                  int64
	minValue                    int64
	maxValue                    int64
}

// NewHistogram builds a histogram tracking values from lowest to highest with
// sigFigs significant figures of resolution.
// lowest must be at least 1, highest at least twice lowest, and sigFigs between 1 and 5.
func NewHistogram(lowest, highest int64, sigFigs int) *Histogram {
	if lowest < 1 {
		lowest = 1
	}
	if highest < 2*lowest {
		highest = 2 * lowest
	}
	if sigFigs < 1 {
		sigFigs = 1
	}
	if sigFigs > 5 {
		sigFigs = 5
	}

	largest := int64(2 * math.Pow10(sigFigs))
	subBucketCountMagnitude := max(uint32(math.Ceil(math.Log2(float64(largest)))), 1)
	unitMagnitude := uint32(math.Floor(math.Log2(float64(lowest))))

	subBucketHalfCountMagnitude := subBucketCountMagnitude - 1
	subBucketCount := int32(1) << (subBucketHalfCountMagnitude + 1)
	subBucketHalfCount := subBucketCount / 2
	subBucketMask := (int64(subBucketCount) - 1) << unitMagnitude

	smallestUntrackable := int64(subBucketCount) << unitMagnitude
	bucketCount := int32(1)
	for smallestUntrackable < highest {
		if smallestUntrackable > math.MaxInt64/2 {
			bucketCount++
			break
		}
		smallestUntrackable <<= 1
		bucketCount++
	}

	countsLen := (bucketCount + 1) * (subBucketCount / 2)
	return &Histogram{
		lowest:                      lowest,
		highest:                     highest,
		sigFigs:                     sigFigs,
		unitMagnitude:               unitMagnitude,
		subBucketHalfCount:          subBucketHalfCount,
		subBucketHalfCountMagnitude: subBucketHalfCountMagnitude,
		subBucketCount:              subBucketCount,
		subBucketMask:               subBucketMask,
		bucketCount:                 bucketCount,
		counts:                      make([]int64, countsLen),
		minValue:                    math.MaxInt64,
	}
}

// NewLatencyHistogram returns a histogram sized for request latencies from one
// nanosecond up to one minute with three significant figures.
func NewLatencyHistogram() *Histogram {
	return NewHistogram(1, int64(60*1e9), 3)
}

func (h *Histogram) bucketIndex(value int64) int32 {
	pow2ceiling := int32(bitLen(value | h.subBucketMask))
	return pow2ceiling - int32(h.unitMagnitude) - int32(bitLen(int64(h.subBucketCount))-1)
}

func (h *Histogram) subBucketIndex(value int64, bucketIndex int32) int32 {
	return int32(value >> (uint32(bucketIndex) + h.unitMagnitude))
}

func (h *Histogram) countsIndex(bucketIndex, subBucketIndex int32) int32 {
	bucketBaseIndex := (bucketIndex + 1) << h.subBucketHalfCountMagnitude
	offsetInBucket := subBucketIndex - h.subBucketHalfCount
	return bucketBaseIndex + offsetInBucket
}

func (h *Histogram) indexFor(value int64) int32 {
	if value < 0 {
		return -1
	}
	bi := h.bucketIndex(value)
	sbi := h.subBucketIndex(value, bi)
	if bi < 0 || bi >= h.bucketCount || sbi < 0 || sbi >= h.subBucketCount {
		return -1
	}
	idx := h.countsIndex(bi, sbi)
	if idx < 0 || idx >= int32(len(h.counts)) {
		return -1
	}
	return idx
}

// RecordValue adds one sample.
// A value above the tracked range is clamped to the top, so a stall is counted rather than dropped.
func (h *Histogram) RecordValue(value int64) {
	if value > h.highest {
		value = h.highest
	}
	if value < 0 {
		value = 0
	}
	idx := h.indexFor(value)
	if idx < 0 {
		return
	}
	h.counts[idx]++
	h.totalCount++
	if value > h.maxValue {
		h.maxValue = value
	}
	if value < h.minValue {
		h.minValue = value
	}
}

// RecordValues adds the same value count times, used by coordinated-omission
// correction to backfill missed samples cheaply.
func (h *Histogram) RecordValues(value, count int64) {
	if count <= 0 {
		return
	}
	if value > h.highest {
		value = h.highest
	}
	if value < 0 {
		value = 0
	}
	idx := h.indexFor(value)
	if idx < 0 {
		return
	}
	h.counts[idx] += count
	h.totalCount += count
	if value > h.maxValue {
		h.maxValue = value
	}
	if value < h.minValue {
		h.minValue = value
	}
}

// RecordCorrectedValue records value and, when it overshoots expectedInterval,
// backfills the samples a stalled client would have missed.
// This is the standard coordinated-omission correction: if a single operation took
// far longer than the steady-state interval, the requests that should have been issued
// during the stall are synthesized at decreasing latencies.
func (h *Histogram) RecordCorrectedValue(value, expectedInterval int64) {
	h.RecordValue(value)
	if expectedInterval <= 0 || value <= expectedInterval {
		return
	}
	missing := value - expectedInterval
	for missing >= expectedInterval {
		h.RecordValue(missing)
		missing -= expectedInterval
	}
}

func (h *Histogram) valueFromIndex(index int32) int64 {
	bucketIndex := (index >> h.subBucketHalfCountMagnitude) - 1
	subBucketIndex := (index & (h.subBucketHalfCount - 1)) + h.subBucketHalfCount
	if bucketIndex < 0 {
		subBucketIndex -= h.subBucketHalfCount
		bucketIndex = 0
	}
	return int64(subBucketIndex) << (uint32(bucketIndex) + h.unitMagnitude)
}

// ValueAtPercentile returns the value at the given percentile in [0,100].
func (h *Histogram) ValueAtPercentile(p float64) int64 {
	if h.totalCount == 0 {
		return 0
	}
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	target := max(int64(math.Ceil((p/100.0)*float64(h.totalCount))), 1)
	var seen int64
	for i := int32(0); i < int32(len(h.counts)); i++ {
		seen += h.counts[i]
		if seen >= target {
			return h.highEquivalentValue(h.valueFromIndex(i))
		}
	}
	return h.maxValue
}

func (h *Histogram) highEquivalentValue(value int64) int64 {
	bi := h.bucketIndex(value)
	sbi := h.subBucketIndex(value, bi)
	lowest := int64(sbi) << (uint32(bi) + h.unitMagnitude)
	sizeOfStep := int64(1) << (uint32(bi) + h.unitMagnitude)
	return lowest + sizeOfStep - 1
}

// TotalCount returns the number of recorded samples.
func (h *Histogram) TotalCount() int64 { return h.totalCount }

// Min returns the smallest recorded value, or 0 if empty.
func (h *Histogram) Min() int64 {
	if h.totalCount == 0 {
		return 0
	}
	return h.minValue
}

// Max returns the largest recorded value.
func (h *Histogram) Max() int64 { return h.maxValue }

// Mean returns the arithmetic mean of the recorded values.
func (h *Histogram) Mean() float64 {
	if h.totalCount == 0 {
		return 0
	}
	var total float64
	for i := int32(0); i < int32(len(h.counts)); i++ {
		if h.counts[i] != 0 {
			total += float64(h.counts[i]) * float64(h.medianEquivalentValue(h.valueFromIndex(i)))
		}
	}
	return total / float64(h.totalCount)
}

func (h *Histogram) medianEquivalentValue(value int64) int64 {
	bi := h.bucketIndex(value)
	sizeOfStep := int64(1) << (uint32(bi) + h.unitMagnitude)
	return h.lowEquivalentValue(value) + sizeOfStep/2
}

func (h *Histogram) lowEquivalentValue(value int64) int64 {
	bi := h.bucketIndex(value)
	sbi := h.subBucketIndex(value, bi)
	return int64(sbi) << (uint32(bi) + h.unitMagnitude)
}

// Merge folds another histogram of the same shape into this one, used to combine
// per-connection histograms into one result.
func (h *Histogram) Merge(other *Histogram) {
	if other == nil || len(other.counts) != len(h.counts) {
		return
	}
	for i := range other.counts {
		h.counts[i] += other.counts[i]
	}
	h.totalCount += other.totalCount
	if other.maxValue > h.maxValue {
		h.maxValue = other.maxValue
	}
	if other.totalCount > 0 && other.minValue < h.minValue {
		h.minValue = other.minValue
	}
}

// OutputPercentileDistribution writes a percentile table for human reading.
// The unitRatio divides recorded values, so passing 1000 prints microseconds from a nanosecond histogram.
func (h *Histogram) OutputPercentileDistribution(w io.Writer, unitRatio float64) {
	pcts := []float64{0, 50, 75, 90, 95, 99, 99.9, 99.99, 100}
	_, _ = fmt.Fprintf(w, "%12s %12s\n", "Percentile", "Value")
	for _, p := range pcts {
		v := float64(h.ValueAtPercentile(p)) / unitRatio
		_, _ = fmt.Fprintf(w, "%12.3f %12.3f\n", p, v)
	}
}

func bitLen(value int64) int {
	n := 0
	for value > 0 {
		n++
		value >>= 1
	}
	return n
}
