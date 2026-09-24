package services

import (
	"math"
	"testing"
)

func TestRainIncrement(t *testing.T) {
	cases := []struct {
		name      string
		newValue  float64
		hasLast   bool
		lastValue float64
		want      float64
	}{
		{"首条记录按读数本身计", 3.2, false, 0, 3.2},
		{"累计值上升取差值", 10.0, true, 7.5, 2.5},
		{"相同累计值重复上报记零", 10.0, true, 10.0, 0},
		{"读数回落按雨量计重置取新值", 1.0, true, 10.0, 1.0},
		{"从零重新累加后继续增长", 12.0, true, 1.0, 11.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rainIncrement(tc.newValue, tc.hasLast, tc.lastValue)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("rainIncrement(%v, %v, %v) = %v, want %v",
					tc.newValue, tc.hasLast, tc.lastValue, got, tc.want)
			}
		})
	}
}
