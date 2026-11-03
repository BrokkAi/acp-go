package serdewire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"testing"
)

// The generator below builds JSON aimed at every rule Encode implements:
// escapes serde_json writes and ones it does not, lone surrogates and
// invalid UTF-8, every number form, repeated and near-identical keys, large
// objects, and deep nesting.

type gen struct {
	r        *rand.Rand
	maxDepth int
}

var strPieces = []string{
	"a", "b", "k", "x", " ", "<", ">", "&", " ", " ", "\x7f", "é", "😀", "\"", "\\", "/",
	`\"`, `\\`, `\/`, `\b`, `\f`, `\n`, `\r`, `\t`, `\u0000`, `\u001f`, `\u001F`, `\u0008`, `\u000a`, `\u000A`,
	`\u007f`, `a`, ` `, `<`, `&`, `😀`, `😀`, `é`, `é`,
	`\u0001`, `\u0019`, `\u001a`, `\u000b`, `\u000c`, `\u000d`, `\u0009`, `￿`, `�`, `\u0080`, `߿`, `ࠀ`,
	"abcdefgh", "abcdefghijklmnop", strings.Repeat("z", 17), "ÿ", "ࠀ",
}
var rarePieces = []string{`\ud800`, `\udc00`, `\ud800A`, `\ud800\ud800`, "\xff", "\xc3"}

var nums = []string{
	"0", "-0", "1", "-1", "100", "1.0", "-0.0", "1e2", "1E2", "1e+2", "1e-7", "1E-7", "0.1", "2.50",
	"123456789012345678", "-123456789012345678", "999999999999999999", "1234567890123456789",
	"9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809",
	"18446744073709551615", "18446744073709551616", "123456789012345678901", "123456789.123456789",
	"1.5e300", "5e-324", "2.2250738585072014e-308", "1e21", "1e20", "0.000001", "1e-6", "1e400", "-1e400",
	"0e0", "-0e0", "0.0", "1.7976931348623157e308", "4.9e-324", "1e-400", "0.30000000000000004",
	"100000000000000000000", "-100000000000000000000", "1e15", "1e16", "1e17",
}

func (g *gen) str(key bool) string {
	var sb strings.Builder
	sb.WriteByte('"')
	n := g.r.Intn(6)
	if g.r.Intn(10) == 0 {
		n = 20 + g.r.Intn(40)
	}
	for i := 0; i < n; i++ {
		if g.r.Intn(200) == 0 {
			sb.WriteString(rarePieces[g.r.Intn(len(rarePieces))])
		} else if g.r.Intn(3) == 0 {
			sb.WriteByte(byte('a' + g.r.Intn(4)))
		} else {
			sb.WriteString(strPieces[g.r.Intn(len(strPieces))])
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

func (g *gen) num() string {
	if g.r.Intn(4) == 0 {
		// random integer of random length
		n := 1 + g.r.Intn(25)
		var sb strings.Builder
		if g.r.Intn(2) == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(byte('1' + g.r.Intn(9)))
		for i := 1; i < n; i++ {
			sb.WriteByte(byte('0' + g.r.Intn(10)))
		}
		if g.r.Intn(3) == 0 {
			sb.WriteString("." + strconv.Itoa(g.r.Intn(100000)))
		}
		if g.r.Intn(4) == 0 {
			sb.WriteString([]string{"e", "E", "e+", "e-", "E-"}[g.r.Intn(5)] + strconv.Itoa(g.r.Intn(330)))
		}
		return sb.String()
	}
	if g.r.Intn(5) == 0 {
		return strconv.FormatFloat(g.r.NormFloat64()*float64(g.r.Int63()), 'g', -1, 64)
	}
	return nums[g.r.Intn(len(nums))]
}

var keyPool = []string{`"a"`, `"b"`, `"a"`, `"\/"`, `"/"`, `"aaaaaaaaXbbbbbbbb"`, `"aaaaaaaaYbbbbbbbb"`, `"aaaaaaaaZZbbbbbbbb"`, "\"k <\"", `"k <"`, `"é"`, `"é"`, `"\n"`, `"\u000a"`}

func (g *gen) value(depth int) string {
	k := g.r.Intn(10)
	if depth >= g.maxDepth {
		k = g.r.Intn(4)
	}
	switch k {
	case 0:
		return []string{"null", "true", "false"}[g.r.Intn(3)]
	case 1, 2:
		return g.num()
	case 3:
		return g.str(false)
	case 4, 5, 6:
		n := g.r.Intn(5)
		if g.r.Intn(15) == 0 {
			n = 60 + g.r.Intn(10)
		}
		var sb strings.Builder
		sb.WriteByte('{')
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			if g.r.Intn(3) == 0 {
				sb.WriteString(keyPool[g.r.Intn(len(keyPool))])
			} else if n > 10 && g.r.Intn(2) == 0 {
				fmt.Fprintf(&sb, `"m%d"`, g.r.Intn(n*2))
			} else {
				sb.WriteString(g.str(true))
			}
			sb.WriteByte(':')
			sb.WriteString(g.value(depth + 1))
		}
		sb.WriteByte('}')
		return sb.String()
	default:
		n := g.r.Intn(5)
		var sb strings.Builder
		sb.WriteByte('[')
		for i := 0; i < n; i++ {
			if i > 0 {
				sb.WriteByte(',')
			}
			sb.WriteString(g.value(depth + 1))
		}
		sb.WriteByte(']')
		return sb.String()
	}
}

func (g *gen) deep() string {
	d := 120 + g.r.Intn(12)
	var sb strings.Builder
	closers := make([]byte, 0, d)
	for i := 0; i < d; i++ {
		if g.r.Intn(2) == 0 {
			sb.WriteString(`{"a":`)
			closers = append(closers, '}')
		} else {
			sb.WriteByte('[')
			closers = append(closers, ']')
		}
	}
	sb.WriteString(g.value(g.maxDepth))
	for i := len(closers) - 1; i >= 0; i-- {
		sb.WriteByte(closers[i])
	}
	return sb.String()
}

// TestEncodeMatchesTheGeneralPathOnGeneratedJSON checks Encode against the
// general path on generated JSON, with a fixed seed.
func TestEncodeMatchesTheGeneralPathOnGeneratedJSON(t *testing.T) {
	g := &gen{r: rand.New(rand.NewSource(1)), maxDepth: 4}
	counts := map[form]int{}
	for i := 0; i < 50000; i++ {
		src := g.value(0)
		if g.r.Intn(50) == 0 {
			src = g.deep()
		}
		var compact bytes.Buffer
		if json.Compact(&compact, []byte(src)) != nil {
			continue
		}
		b := compact.Bytes()
		counts[classify(b)]++
		want, ok := slowPath(t, b)
		got, err := Encode(b)
		if !ok {
			if err != ErrUnrepresentable {
				t.Fatalf("Encode(%q) = %q, %v; want ErrUnrepresentable", b, got, err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Encode(%q) = %q, %v; want %q", b, got, err, want)
		}
	}
	if counts[serdeExact] == 0 || counts[serdeTokens] == 0 || counts[serdeReparse] == 0 {
		t.Fatalf("a path was never taken: %v", counts)
	}
}
