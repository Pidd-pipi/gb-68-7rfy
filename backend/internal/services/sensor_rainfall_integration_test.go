package services

import (
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"irrigation/internal/models"
	"irrigation/pkg/database"
)

// setupTestDB 使用内存 SQLite 搭建测试库（PostgreSQL 专有的咨询锁替换为空操作）。
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared&_fk=1"), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}

	if err := db.AutoMigrate(&models.Device{}, &models.SensorData{}); err != nil {
		t.Fatalf("迁移测试数据库失败: %v", err)
	}

	database.DB = db
	lockRainfallDeviceFunc = func(tx *gorm.DB, deviceID uint) error { return nil }

	t.Cleanup(func() {
		lockRainfallDeviceFunc = func(tx *gorm.DB, deviceID uint) error {
			return tx.Exec("SELECT pg_advisory_xact_lock(?)", rainfallLockKey(deviceID)).Error
		}
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func createRainSensor(t *testing.T) *models.Device {
	t.Helper()
	// SQLite 测试驱动不支持 jsonb map 字段，这里用原生 SQL 插入设备记录。
	if err := database.DB.Exec(
		`INSERT INTO devices (name, type, serial_number, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"雨量计-1", string(models.DeviceTypeRainSensor), "RAIN-001",
		string(models.DeviceStatusOnline), time.Now(), time.Now(),
	).Error; err != nil {
		t.Fatalf("创建设备失败: %v", err)
	}

	var device models.Device
	if err := database.DB.Where("serial_number = ?", "RAIN-001").First(&device).Error; err != nil {
		t.Fatalf("查询设备失败: %v", err)
	}
	return &device
}

func TestRainfallReportingLifecycle(t *testing.T) {
	s := setupTestDB(t)
	_ = s
	svc := NewSensorService()

	// 设备不存在时拒绝
	missing := &models.SensorData{DeviceID: 999, DataType: models.DataTypeRainfall, Value: 1, Timestamp: time.Now()}
	if err := svc.CreateSensorData(missing); err != ErrDeviceNotFound {
		t.Fatalf("设备不存在应返回 ErrDeviceNotFound，实际: %v", err)
	}

	device := createRainSensor(t)

	exists, err := svc.DeviceExists(device.ID)
	if err != nil || !exists {
		t.Fatalf("设备应存在: exists=%v err=%v", exists, err)
	}

	// 以 30 分钟前为基准，保证所有读数都落在“最近两小时”窗口内（时间不能晚于查询时刻）。
	now := time.Now().Add(-30 * time.Minute)
	report := func(value float64, offset time.Duration) *models.SensorData {
		d := &models.SensorData{
			DeviceID:  device.ID,
			DataType:  models.DataTypeRainfall,
			Value:     value,
			Timestamp: now.Add(offset),
		}
		if err := svc.CreateSensorData(d); err != nil {
			t.Fatalf("上报雨量 %.1f 失败: %v", value, err)
		}
		return d
	}

	first := report(10, 0)
	if first.RainfallIncrement == nil || *first.RainfallIncrement != 10 {
		t.Fatalf("首条记录增量应为 10，实际: %v", first.RainfallIncrement)
	}
	if first.Unit != models.RainfallUnit {
		t.Fatalf("雨量单位默认应为 mm，实际: %s", first.Unit)
	}

	dup := report(10, time.Minute)
	if dup.RainfallIncrement == nil || *dup.RainfallIncrement != 0 {
		t.Fatalf("同一累计值重复上报增量应为 0，实际: %v", dup.RainfallIncrement)
	}

	grown := report(13, 2*time.Minute)
	if grown.RainfallIncrement == nil || *grown.RainfallIncrement != 3 {
		t.Fatalf("读数增长增量应为 3，实际: %v", grown.RainfallIncrement)
	}

	// 时间早于上一条拒绝
	old := &models.SensorData{
		DeviceID:  device.ID,
		DataType:  models.DataTypeRainfall,
		Value:     14,
		Timestamp: now.Add(90 * time.Second),
	}
	if err := svc.CreateSensorData(old); err != ErrTimestampOutOfOrder {
		t.Fatalf("乱序上报应返回 ErrTimestampOutOfOrder，实际: %v", err)
	}

	// 负值拒绝
	negative := &models.SensorData{
		DeviceID:  device.ID,
		DataType:  models.DataTypeRainfall,
		Value:     -1,
		Timestamp: now.Add(3 * time.Minute),
	}
	if err := svc.CreateSensorData(negative); err != ErrNegativeRainfall {
		t.Fatalf("负值应返回 ErrNegativeRainfall，实际: %v", err)
	}

	// 读数回落：按雨量计重置，采用新值 2
	reset := report(2, 3*time.Minute)
	if reset.RainfallIncrement == nil || *reset.RainfallIncrement != 2 {
		t.Fatalf("重置后增量应采用新值 2，实际: %v", reset.RainfallIncrement)
	}

	after := report(5, 4*time.Minute)
	if after.RainfallIncrement == nil || *after.RainfallIncrement != 3 {
		t.Fatalf("重置后增长增量应为 3，实际: %v", after.RainfallIncrement)
	}

	// 近两小时降雨量按增量求和：10 + 0 + 3 + 2 + 3 = 18（快照直接相加会得到 40）
	summary, err := svc.GetRainfallLast2Hours(device.ID)
	if err != nil {
		t.Fatalf("查询近两小时降雨失败: %v", err)
	}
	if summary.Rainfall != 18 {
		t.Fatalf("近两小时降雨应为 18mm，实际: %vmm", summary.Rainfall)
	}

	total, err := svc.CheckRecentRainfall(device.ID, 2*time.Hour)
	if err != nil || total != 18 {
		t.Fatalf("CheckRecentRainfall 应为 18，实际: %v err=%v", total, err)
	}

	// 非雨量数据不受影响，增量字段保持为空
	soil := &models.SensorData{
		DeviceID:  device.ID,
		DataType:  "soil_humidity",
		Value:     42.5,
		Timestamp: now.Add(5 * time.Minute),
	}
	if err := svc.CreateSensorData(soil); err != nil {
		t.Fatalf("非雨量数据上报失败: %v", err)
	}
	if soil.RainfallIncrement != nil {
		t.Fatalf("非雨量数据不应写入降雨增量，实际: %v", *soil.RainfallIncrement)
	}

	// 历史明细仍可查询且包含累计值与增量
	history, err := svc.GetSensorHistory(device.ID, time.Time{}, time.Time{}, 100)
	if err != nil {
		t.Fatalf("查询历史明细失败: %v", err)
	}
	var rainRows int
	for _, h := range history {
		if h.DataType == models.DataTypeRainfall {
			rainRows++
		}
	}
	if rainRows != 5 {
		t.Fatalf("雨量历史明细应有 5 条，实际: %d", rainRows)
	}
}

// 批量上报中雨量记录也按增量落库，且整批失败时回滚。
func TestRainfallBatchReporting(t *testing.T) {
	setupTestDB(t)
	svc := NewSensorService()
	device := createRainSensor(t)

	now := time.Now().Add(-30 * time.Minute)
	batch := []models.SensorData{
		{DeviceID: device.ID, DataType: models.DataTypeRainfall, Value: 5, Timestamp: now},
		{DeviceID: device.ID, DataType: models.DataTypeRainfall, Value: 5, Timestamp: now.Add(time.Minute)},
		{DeviceID: device.ID, DataType: models.DataTypeRainfall, Value: 9, Timestamp: now.Add(2 * time.Minute)},
	}
	if err := svc.BatchCreateSensorData(batch); err != nil {
		t.Fatalf("批量上报失败: %v", err)
	}
	if *batch[0].RainfallIncrement != 5 || *batch[1].RainfallIncrement != 0 || *batch[2].RainfallIncrement != 4 {
		t.Fatalf("批量增量应为 [5 0 4]，实际: %v %v %v",
			batch[0].RainfallIncrement, batch[1].RainfallIncrement, batch[2].RainfallIncrement)
	}

	// 批量中包含不存在的设备，整批拒绝（事务回滚，不写入任何记录）
	bad := []models.SensorData{
		{DeviceID: device.ID, DataType: models.DataTypeRainfall, Value: 10, Timestamp: now.Add(3 * time.Minute)},
		{DeviceID: 404, DataType: models.DataTypeRainfall, Value: 1, Timestamp: now.Add(4 * time.Minute)},
	}
	if err := svc.BatchCreateSensorData(bad); err != ErrDeviceNotFound {
		t.Fatalf("批量含不存在设备应返回 ErrDeviceNotFound，实际: %v", err)
	}

	var count int64
	database.DB.Model(&models.SensorData{}).
		Where("device_id = ? AND data_type = ?", device.ID, models.DataTypeRainfall).
		Count(&count)
	if count != 3 {
		t.Fatalf("失败的批量应回滚，雨量记录仍应为 3 条，实际: %d", count)
	}
}
