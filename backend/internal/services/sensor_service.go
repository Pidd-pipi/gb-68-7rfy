package services

import (
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"
	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// 雨量上报相关的可预期错误，控制器据此返回 4xx。
var (
	ErrDeviceNotFound      = errors.New("device not found")
	ErrTimestampOutOfOrder = errors.New("timestamp is earlier than the latest record")
	ErrNegativeRainfall    = errors.New("rainfall reading must not be negative")
)

type SensorService struct{}

func NewSensorService() *SensorService {
	return &SensorService{}
}

func (s *SensorService) CreateSensorData(data *models.SensorData) error {
	if data.Timestamp.IsZero() {
		data.Timestamp = time.Now()
	}

	// 雨量传感器上报的是累计毫米数，必须按增量落库，避免快照被重复累加。
	if data.DataType == models.DataTypeRainfall {
		return s.ReportRainfall(data)
	}

	return database.DB.Create(data).Error
}

// ReportRainfall 处理雨量传感器的累计读数上报。
//
// 设备上报的 value 为雨量计自上次清零以来的累计毫米数：
//   - 首条记录没有旧累计可参照，按重置语义采用新值本身；
//   - 读数回落（new < last）视为雨量计重置，增量采用新值，
//     旧累计读数不参与求和；
//   - 读数高于上一条时，增量为两者之差；
//   - 与上一条累计值相同（重复上报）时增量记 0；
//   - 时间早于该设备上一条记录、设备不存在或读数为负时拒绝。
//
// 使用按设备维度的咨询锁串行化同一设备的并发上报，保证“上一条”的判断可靠。
func (s *SensorService) ReportRainfall(data *models.SensorData) error {
	if data.Value < 0 {
		return ErrNegativeRainfall
	}

	return database.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockRainfallDevice(tx, data.DeviceID); err != nil {
			return err
		}

		record, err := s.appendRainfall(tx, data)
		if err != nil {
			return err
		}
		*data = *record
		return nil
	})
}

// appendRainfall 必须在持有该设备咨询锁的事务中调用。
func (s *SensorService) appendRainfall(tx *gorm.DB, data *models.SensorData) (*models.SensorData, error) {
	var deviceCount int64
	if err := tx.Model(&models.Device{}).Where("id = ?", data.DeviceID).Count(&deviceCount).Error; err != nil {
		return nil, err
	}
	if deviceCount == 0 {
		return nil, ErrDeviceNotFound
	}

	ts := data.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	var last models.SensorData
	err := tx.Where("device_id = ? AND data_type = ?", data.DeviceID, models.DataTypeRainfall).
		Order("timestamp DESC, id DESC").First(&last).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var increment float64
	if err == nil {
		increment, err = computeRainfallIncrement(last.Value, data.Value, ts, last.Timestamp)
		if err != nil {
			return nil, err
		}
	} else {
		// 首条记录没有可参照的旧累计，按重置语义采用新值本身。
		increment = data.Value
	}

	record := &models.SensorData{
		DeviceID:          data.DeviceID,
		DataType:          models.DataTypeRainfall,
		Value:             data.Value,
		RainfallIncrement: &increment,
		Unit:              rainfallUnitOr(data.Unit),
		Timestamp:         ts,
	}
	if err := tx.Create(record).Error; err != nil {
		return nil, err
	}
	return record, nil
}

// computeRainfallIncrement 根据上一条与本次累计读数计算增量。
//   - 时间戳早于上一条：拒绝；
//   - 读数回落（new < last）：按雨量计重置处理，旧累计不参与求和，采用新值；
//   - 读数相同：重复上报，记零增量；
//   - 读数增长：取差值。
func computeRainfallIncrement(lastValue, newValue float64, newTime, lastTime time.Time) (float64, error) {
	if newTime.Before(lastTime) {
		return 0, ErrTimestampOutOfOrder
	}
	switch {
	case newValue < lastValue:
		return newValue, nil
	case newValue == lastValue:
		return 0, nil
	default:
		return newValue - lastValue, nil
	}
}

// lockRainfallDevice 获取按设备维度的事务级咨询锁，锁在事务结束（提交/回滚）时自动释放。
// 使用包级变量便于在非 PostgreSQL 环境（如测试）下替换为空操作。
var lockRainfallDeviceFunc = func(tx *gorm.DB, deviceID uint) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", rainfallLockKey(deviceID)).Error
}

func lockRainfallDevice(tx *gorm.DB, deviceID uint) error {
	return lockRainfallDeviceFunc(tx, deviceID)
}

// rainfallLockKey 将设备 ID 映射到咨询锁命名空间内的键。
func rainfallLockKey(deviceID uint) int64 {
	return int64(deviceID)
}

func rainfallUnitOr(unit string) string {
	if unit != "" {
		return unit
	}
	return models.RainfallUnit
}

func (s *SensorService) BatchCreateSensorData(dataList []models.SensorData) error {
	if len(dataList) == 0 {
		return nil
	}
	now := time.Now()
	for i := range dataList {
		if dataList[i].Timestamp.IsZero() {
			dataList[i].Timestamp = now
		}
	}

	return database.DB.Transaction(func(tx *gorm.DB) error {
		// 雨量记录同样按增量落库。先收集涉及的设备并按固定顺序加锁，避免并发/多设备时死锁。
		rainDeviceSet := make(map[uint]struct{})
		for i := range dataList {
			if dataList[i].DataType == models.DataTypeRainfall {
				if dataList[i].Value < 0 {
					return ErrNegativeRainfall
				}
				rainDeviceSet[dataList[i].DeviceID] = struct{}{}
			}
		}

		rainDevices := make([]uint, 0, len(rainDeviceSet))
		for id := range rainDeviceSet {
			rainDevices = append(rainDevices, id)
		}
		sort.Slice(rainDevices, func(i, j int) bool { return rainDevices[i] < rainDevices[j] })
		for _, id := range rainDevices {
			if err := lockRainfallDevice(tx, id); err != nil {
				return err
			}
		}

		for i := range dataList {
			if dataList[i].DataType == models.DataTypeRainfall {
				record, err := s.appendRainfall(tx, &dataList[i])
				if err != nil {
					return err
				}
				dataList[i] = *record
			} else {
				if err := tx.Create(&dataList[i]).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *SensorService) GetSensorHistory(deviceID uint, startTime, endTime time.Time, limit int) ([]models.SensorData, error) {
	var data []models.SensorData
	query := database.DB.Where("device_id = ?", deviceID)

	if !startTime.IsZero() {
		query = query.Where("timestamp >= ?", startTime)
	}
	if !endTime.IsZero() {
		query = query.Where("timestamp <= ?", endTime)
	}

	if limit > 0 {
		query = query.Limit(limit)
	}

	if err := query.Order("timestamp DESC").Find(&data).Error; err != nil {
		return nil, err
	}
	return data, nil
}

func (s *SensorService) GetLatestSensorData(deviceID uint, dataType string) (*models.SensorData, error) {
	var data models.SensorData
	query := database.DB.Where("device_id = ?", deviceID)
	if dataType != "" {
		query = query.Where("data_type = ?", dataType)
	}
	err := query.Order("timestamp DESC").First(&data).Error
	if err == gorm.ErrRecordNotFound {
		return nil, nil
	}
	return &data, err
}

// DeviceExists 判断设备是否存在。
func (s *SensorService) DeviceExists(deviceID uint) (bool, error) {
	var count int64
	if err := database.DB.Model(&models.Device{}).Where("id = ?", deviceID).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *SensorService) GetAverageHumidity(zoneID uint, duration time.Duration) (*float64, error) {
	var avg *float64
	subQuery := database.DB.Model(&models.Device{}).
		Select("id").
		Where("zone_id = ? AND type = ?", zoneID, models.DeviceTypeSoilSensor)

	err := database.DB.Model(&models.SensorData{}).
		Select("AVG(value)").
		Where("device_id IN (?) AND timestamp >= ?", subQuery, time.Now().Add(-duration)).
		Scan(&avg).Error

	return avg, err
}

// RainfallSummary 最近一段时间的降雨量汇总。
type RainfallSummary struct {
	DeviceID uint      `json:"device_id"`
	Window   string    `json:"window"`
	Start    time.Time `json:"start_time"`
	End      time.Time `json:"end_time"`
	Rainfall float64   `json:"rainfall"` // 窗口内各次增量之和（毫米）
}

// GetRainfall 返回指定设备最近 window 时间内的降雨量，按各次上报的增量求和。
func (s *SensorService) GetRainfall(deviceID uint, window time.Duration) (*RainfallSummary, error) {
	end := time.Now()
	start := end.Add(-window)

	var total float64
	err := database.DB.Model(&models.SensorData{}).
		Select("COALESCE(SUM(rainfall_increment), 0)").
		Where("device_id = ? AND data_type = ? AND timestamp >= ? AND timestamp <= ?",
			deviceID, models.DataTypeRainfall, start, end).
		Scan(&total).Error
	if err != nil {
		return nil, err
	}

	return &RainfallSummary{
		DeviceID: deviceID,
		Window:   window.String(),
		Start:    start,
		End:      end,
		Rainfall: total,
	}, nil
}

// GetRainfallLast2Hours 提供给“按设备查看最近两小时降雨量”接口使用。
func (s *SensorService) GetRainfallLast2Hours(deviceID uint) (*RainfallSummary, error) {
	return s.GetRainfall(deviceID, models.RainfallWindow)
}

// CheckRecentRainfall 返回最近一段时间的降雨增量合计（毫米），供调度引擎避雨判断。
func (s *SensorService) CheckRecentRainfall(sensorID uint, duration time.Duration) (float64, error) {
	summary, err := s.GetRainfall(sensorID, duration)
	if err != nil {
		return 0, err
	}
	return summary.Rainfall, nil
}
