package services

import (
	"errors"
	"sort"
	"time"

	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

var (
	// ErrRainDeviceNotFound 上报雨量数据的设备不存在
	ErrRainDeviceNotFound = errors.New("device not found")
	// ErrRainTimestampStale 上报时间早于该设备上一条雨量记录
	ErrRainTimestampStale = errors.New("rainfall timestamp is earlier than the latest record")
	// ErrRainValueNegative 雨量累计值不允许为负
	ErrRainValueNegative = errors.New("rainfall cumulative value must not be negative")
)

type SensorService struct{}

func NewSensorService() *SensorService {
	return &SensorService{}
}

// rainfallLockNamespace 雨量录入串行化用的 advisory lock 命名空间常量
const rainfallLockNamespace int64 = 0x5241494E // "RAIN"

func (s *SensorService) CreateSensorData(data *models.SensorData) error {
	if data.Timestamp.IsZero() {
		data.Timestamp = time.Now()
	}

	if data.DataType == models.DataTypeRainfall {
		return database.DB.Transaction(func(tx *gorm.DB) error {
			return s.createRainfallData(tx, data)
		})
	}
	return database.DB.Create(data).Error
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
		// 按设备 ID 排序后统一加锁，避免并发批量上报时因加锁顺序不一致而死锁
		rainDeviceSet := make(map[uint]struct{})
		for i := range dataList {
			if dataList[i].DataType == models.DataTypeRainfall {
				rainDeviceSet[dataList[i].DeviceID] = struct{}{}
			}
		}
		rainDeviceIDs := make([]int64, 0, len(rainDeviceSet))
		for id := range rainDeviceSet {
			rainDeviceIDs = append(rainDeviceIDs, int64(id))
		}
		sort.Slice(rainDeviceIDs, func(i, j int) bool { return rainDeviceIDs[i] < rainDeviceIDs[j] })
		for _, id := range rainDeviceIDs {
			if err := tx.Exec("SELECT pg_advisory_xact_lock(?, ?)", rainfallLockNamespace, id).Error; err != nil {
				return err
			}
		}

		for i := range dataList {
			if dataList[i].DataType == models.DataTypeRainfall {
				if err := s.createRainfallData(tx, &dataList[i]); err != nil {
					return err
				}
			} else if err := tx.Create(&dataList[i]).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// createRainfallData 录入一条雨量累计读数，并保存相对上一条记录的增量。
//
// 规则：
//   - 设备不存在时拒绝；
//   - 上报时间早于该设备上一条雨量记录时拒绝；
//   - 累计值不允许为负；
//   - 累计值上升：增量 = 新值 - 上一条累计值；
//   - 累计值相同（重复上报）：增量记 0；
//   - 累计值回落：视为雨量计重置，采用新值，旧累计不参与求和。
func (s *SensorService) createRainfallData(tx *gorm.DB, data *models.SensorData) error {
	if data.Value < 0 {
		return ErrRainValueNegative
	}

	// 按设备串行化雨量写入，避免并发时两条记录同时把自己当作首条而重复计数
	if err := tx.Exec("SELECT pg_advisory_xact_lock(?, ?)",
		rainfallLockNamespace, int64(data.DeviceID)).Error; err != nil {
		return err
	}

	var device models.Device
	err := tx.Select("id").First(&device, data.DeviceID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrRainDeviceNotFound
	}
	if err != nil {
		return err
	}

	var last models.SensorData
	queryErr := tx.Where("device_id = ? AND data_type = ?", data.DeviceID, models.DataTypeRainfall).
		Order("timestamp DESC, id DESC").
		First(&last).Error
	if queryErr != nil && !errors.Is(queryErr, gorm.ErrRecordNotFound) {
		return queryErr
	}

	if queryErr == nil && data.Timestamp.Before(last.Timestamp) {
		return ErrRainTimestampStale
	}

	increment := rainIncrement(data.Value, queryErr == nil, last.Value)
	data.RainIncrement = &increment

	return tx.Create(data).Error
}

// rainIncrement 计算雨量读数相对上一条的增量。
// hasLast 为 false 表示这是该设备的第一条雨量记录。
//
//   - 首条记录：增量即读数本身（累计值视为从零开始）；
//   - 读数上升：增量 = 新值 - 旧累计值；
//   - 读数相同：增量为 0（重复上报）；
//   - 读数回落：视为雨量计重置，增量取新值，旧累计不参与求和。
func rainIncrement(newValue float64, hasLast bool, lastValue float64) float64 {
	if !hasLast {
		return newValue
	}
	switch {
	case newValue == lastValue:
		return 0
	case newValue < lastValue:
		return newValue
	default:
		return newValue - lastValue
	}
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

// CheckRecentRainfall 统计指定雨量传感器最近 duration 内的降雨量（毫米）。
// 按保存的增量求和；读数重置后的记录只计新值，重复上报记 0。
func (s *SensorService) CheckRecentRainfall(sensorID uint, duration time.Duration) (float64, error) {
	var total float64
	err := database.DB.Model(&models.SensorData{}).
		Select("COALESCE(SUM(rain_increment), 0)").
		Where("device_id = ? AND data_type = ? AND timestamp >= ?",
			sensorID, models.DataTypeRainfall, time.Now().Add(-duration)).
		Scan(&total).Error
	return total, err
}

// RainfallSummary 最近降雨量汇总
type RainfallSummary struct {
	DeviceID  uint    `json:"device_id"`
	Hours     float64 `json:"hours"`
	Rainfall  float64 `json:"rainfall"`
	Unit      string  `json:"unit"`
	StartTime string  `json:"start_time"`
	EndTime   string  `json:"end_time"`
}

// GetRecentRainfall 按设备查看最近 hours 小时内的降雨量（基于增量求和）。
// 设备不存在时返回 ErrRainDeviceNotFound。
func (s *SensorService) GetRecentRainfall(deviceID uint, hours float64) (*RainfallSummary, error) {
	var device models.Device
	err := database.DB.Select("id").First(&device, deviceID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrRainDeviceNotFound
	}
	if err != nil {
		return nil, err
	}

	end := time.Now()
	start := end.Add(-time.Duration(hours * float64(time.Hour)))

	var total float64
	if err := database.DB.Model(&models.SensorData{}).
		Select("COALESCE(SUM(rain_increment), 0)").
		Where("device_id = ? AND data_type = ? AND timestamp >= ?",
			deviceID, models.DataTypeRainfall, start).
		Scan(&total).Error; err != nil {
		return nil, err
	}

	return &RainfallSummary{
		DeviceID:  deviceID,
		Hours:     hours,
		Rainfall:  total,
		Unit:      "mm",
		StartTime: start.Format(time.RFC3339),
		EndTime:   end.Format(time.RFC3339),
	}, nil
}
