package services

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestComputeRainfallIncrement(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		lastValue float64
		newValue  float64
		newTime   time.Time
		lastTime  time.Time
		want      float64
		wantErr   error
	}{
		{
			name:      "读数正常增长，增量为差值",
			lastValue: 12.0,
			newValue:  15.5,
			newTime:   base.Add(time.Minute),
			lastTime:  base,
			want:      3.5,
		},
		{
			name:      "同一累计值重复上报，记零增量",
			lastValue: 15.5,
			newValue:  15.5,
			newTime:   base.Add(time.Minute),
			lastTime:  base,
			want:      0,
		},
		{
			name:      "读数回落按雨量计重置，采用新值，旧累计不参与求和",
			lastValue: 100.0,
			newValue:  4.0,
			newTime:   base.Add(time.Minute),
			lastTime:  base,
			want:      4.0,
		},
		{
			name:      "重置后归零再上报，增量为零",
			lastValue: 0,
			newValue:  0,
			newTime:   base.Add(time.Minute),
			lastTime:  base,
			want:      0,
		},
		{
			name:      "时间早于上一条则拒绝",
			lastValue: 10.0,
			newValue:  12.0,
			newTime:   base.Add(-time.Minute),
			lastTime:  base,
			wantErr:   ErrTimestampOutOfOrder,
		},
		{
			name:      "时间与上一条相同允许（同一时刻补报）",
			lastValue: 10.0,
			newValue:  12.0,
			newTime:   base,
			lastTime:  base,
			want:      2.0,
		},
		{
			name:      "回落至零时增量为零",
			lastValue: 50.0,
			newValue:  0,
			newTime:   base.Add(time.Minute),
			lastTime:  base,
			want:      0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := computeRainfallIncrement(tt.lastValue, tt.newValue, tt.newTime, tt.lastTime)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("期望错误 %v，实际 %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("未期望错误，实际 %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("期望增量 %v，实际 %v", tt.want, got)
			}
		})
	}
}

// 验证连续上报序列按增量累加，而不是快照相加。
func TestRainfallSequenceSumsIncrements(t *testing.T) {
	base := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	readings := []struct {
		value float64
		at    time.Time
	}{
		{10.0, base},
		{10.0, base.Add(time.Minute)}, // 重复上报
		{13.0, base.Add(2 * time.Minute)},
		{13.0, base.Add(3 * time.Minute)}, // 重复上报
		{2.0, base.Add(4 * time.Minute)},  // 雨量计重置
		{5.0, base.Add(5 * time.Minute)},
	}

	var total float64
	var lastValue float64
	var lastTime time.Time
	for i, r := range readings {
		var inc float64
		if i == 0 {
			inc = r.value // 首条按新值本身
		} else {
			var err error
			inc, err = computeRainfallIncrement(lastValue, r.value, r.at, lastTime)
			if err != nil {
				t.Fatalf("第 %d 条不应被拒绝: %v", i, err)
			}
		}
		total += inc
		lastValue, lastTime = r.value, r.at
	}

	// 快照直接相加会得到 10+10+13+13+2+5=53；
	// 正确增量序列为 10+0+3+0+2+3 = 18。
	if math.Abs(total-18.0) > 1e-9 {
		t.Fatalf("期望增量合计 18mm，实际 %vmm", total)
	}

	// 最近两小时避雨门槛：18mm 超过 5mm，应跳过灌溉。
	if !(total > 5.0) {
		t.Fatal("18mm 降雨应触发 5mm 避雨门槛")
	}
}
