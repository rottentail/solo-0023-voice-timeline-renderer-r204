package audiocodec

import "math"

const silenceFloorDBFS = -120.0

// MetricsAccumulator folds peak and RMS statistics over a stream of samples
// with O(1) memory. Feeding it the same samples as Measure yields identical
// results.
type MetricsAccumulator struct {
	peak       float64
	sumSquares float64
	count      int64
}

func (acc *MetricsAccumulator) Add(sample float32) {
	value := float64(sample)
	absolute := math.Abs(value)
	if absolute > acc.peak {
		acc.peak = absolute
	}
	acc.sumSquares += value * value
	acc.count++
}

func (acc *MetricsAccumulator) Metrics() Metrics {
	if acc.count == 0 {
		return Metrics{PeakDBFS: silenceFloorDBFS, RMSDBFS: silenceFloorDBFS}
	}
	rms := math.Sqrt(acc.sumSquares / float64(acc.count))
	return Metrics{
		PeakDBFS: amplitudeDBFS(acc.peak),
		RMSDBFS:  amplitudeDBFS(rms),
	}
}

func Measure(samples []float32) Metrics {
	var acc MetricsAccumulator
	for _, sample := range samples {
		acc.Add(sample)
	}
	return acc.Metrics()
}

func amplitudeDBFS(value float64) float64 {
	if value <= 0 {
		return silenceFloorDBFS
	}
	measured := 20 * math.Log10(value)
	if measured < silenceFloorDBFS {
		return silenceFloorDBFS
	}
	return measured
}

func DBToLinear(db float64) float64 {
	return math.Pow(10, db/20)
}

func LinearToDB(linear float64) float64 {
	return amplitudeDBFS(linear)
}
