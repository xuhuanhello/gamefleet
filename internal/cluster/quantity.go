package cluster

import (
	"math"
	"strconv"
	"strings"
)

// Resources is an amount of CPU and memory.
type Resources struct {
	MilliCPU int64
	Memory   int64 // bytes
}

// Add returns the sum of both amounts.
func (r Resources) Add(o Resources) Resources {
	return Resources{MilliCPU: r.MilliCPU + o.MilliCPU, Memory: r.Memory + o.Memory}
}

func resources(cpu, memory string) Resources {
	return Resources{
		MilliCPU: int64(math.Round(parseQuantity(cpu) * 1000)),
		Memory:   int64(math.Round(parseQuantity(memory))),
	}
}

// Binary suffixes come first so that "Mi" is not read as "M".
var quantitySuffixes = []struct {
	suffix string
	factor float64
}{
	{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50}, {"Ei", 1 << 60},
	{"n", 1e-9}, {"u", 1e-6}, {"m", 1e-3},
	{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15}, {"E", 1e18},
}

// parseQuantity reads a Kubernetes quantity such as "250m", "2", "1941796Ki"
// or "84521331n" in base units. It returns 0 for anything it cannot read.
func parseQuantity(s string) float64 {
	for _, q := range quantitySuffixes {
		if number, ok := strings.CutSuffix(s, q.suffix); ok {
			v, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0
			}
			return v * q.factor
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
