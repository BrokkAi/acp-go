package serdejson

import (
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"
)

// stringFormatFloat is the string-building formatter AppendFloat replaced,
// kept as its oracle.
func stringFormatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		if math.Signbit(f) {
			return "-0.0"
		}
		return "0.0"
	}
	sign := ""
	if f < 0 {
		sign = "-"
		f = -f
	}
	scientific := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(scientific, "e")
	digits := strings.Replace(mantissa, ".", "", 1)
	exponent, _ := strconv.Atoi(exponentText)
	point := exponent + 1
	switch {
	case point > 0 && point <= 16:
		if len(digits) <= point {
			return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
		}
		return sign + digits[:point] + "." + digits[point:]
	case point > -5 && point <= 0:
		return sign + "0." + strings.Repeat("0", -point) + digits
	}
	result := sign + digits[:1]
	if len(digits) > 1 {
		result += "." + digits[1:]
	}
	if exponent >= 0 {
		return result + "e+" + strconv.Itoa(exponent)
	}
	return result + "e" + strconv.Itoa(exponent)
}

func TestAppendFloatMatchesTheStringFormatter(t *testing.T) {
	values := []float64{0, math.Copysign(0, -1), 1, -1, 0.1, 1e15, 1e16, 1e17, 123456789.12345679, 1e-4, 1e-5, 1e-7,
		1.5e300, 5e-324, math.MaxFloat64, math.SmallestNonzeroFloat64, 1.8446744073709552e+19, math.NaN(), math.Inf(1), math.Inf(-1)}
	random := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 200000; i++ {
		values = append(values, math.Float64frombits(random.Uint64()), random.Float64()*math.Pow(10, float64(random.IntN(40)-20)))
	}
	for _, f := range values {
		if got, want := string(AppendFloat(nil, f)), stringFormatFloat(f); got != want {
			t.Fatalf("AppendFloat(%v) = %s, want %s", f, got, want)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() {
		var buf [32]byte
		_ = AppendFloat(buf[:0], 123456.789)
	}); allocs != 0 {
		t.Fatalf("AppendFloat allocates %v times", allocs)
	}
}
