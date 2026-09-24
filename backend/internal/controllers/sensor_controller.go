package controllers

import (
	"errors"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"irrigation/internal/models"
	"irrigation/internal/services"
	"irrigation/pkg/response"
)

type SensorController struct {
	sensorService *services.SensorService
}

func NewSensorController() *SensorController {
	return &SensorController{
		sensorService: services.NewSensorService(),
	}
}

// CreateSensorData godoc
// @Summary 上报传感器数据
// @Description 设备上报传感器数据
// @Tags 传感器数据
// @Accept json
// @Produce json
// @Param request body models.SensorData true "传感器数据"
// @Success 201 {object} models.SensorData
// @Router /api/sensors/data [post]
func (c *SensorController) Create(ctx *gin.Context) {
	var data models.SensorData
	if err := ctx.ShouldBindJSON(&data); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.sensorService.CreateSensorData(&data); err != nil {
		respondSensorError(ctx, err)
		return
	}

	response.Created(ctx, data)
}

// BatchCreateSensorData godoc
// @Summary 批量上报传感器数据
// @Description 批量上报传感器数据
// @Tags 传感器数据
// @Accept json
// @Produce json
// @Param request body []models.SensorData true "传感器数据列表"
// @Success 200 {object} response.Response
// @Router /api/sensors/data/batch [post]
func (c *SensorController) BatchCreate(ctx *gin.Context) {
	var dataList []models.SensorData
	if err := ctx.ShouldBindJSON(&dataList); err != nil {
		response.BadRequest(ctx, "Invalid request body")
		return
	}

	if err := c.sensorService.BatchCreateSensorData(dataList); err != nil {
		respondSensorError(ctx, err)
		return
	}

	response.Success(ctx, nil)
}

// respondSensorError 将雨量录入的业务校验错误映射为对应的 HTTP 状态码
func respondSensorError(ctx *gin.Context, err error) {
	switch {
	case errors.Is(err, services.ErrRainDeviceNotFound):
		response.NotFound(ctx, err.Error())
	case errors.Is(err, services.ErrRainTimestampStale), errors.Is(err, services.ErrRainValueNegative):
		response.BadRequest(ctx, err.Error())
	default:
		response.InternalServerError(ctx, err.Error())
	}
}

// GetSensorHistory godoc
// @Summary 获取传感器历史数据
// @Description 获取指定设备的传感器历史数据
// @Tags 传感器数据
// @Security ApiKeyAuth
// @Produce json
// @Param device_id path int true "设备ID"
// @Param start_time query string false "开始时间 (RFC3339)"
// @Param end_time query string false "结束时间 (RFC3339)"
// @Param limit query int false "返回数量限制" default(100)
// @Success 200 {array} models.SensorData
// @Router /api/sensors/history/{device_id} [get]
func (c *SensorController) GetHistory(ctx *gin.Context) {
	deviceID, _ := strconv.ParseUint(ctx.Param("device_id"), 10, 32)

	var startTime, endTime time.Time
	if startStr := ctx.Query("start_time"); startStr != "" {
		startTime, _ = time.Parse(time.RFC3339, startStr)
	}
	if endStr := ctx.Query("end_time"); endStr != "" {
		endTime, _ = time.Parse(time.RFC3339, endStr)
	}

	limit := 100
	if limitStr := ctx.Query("limit"); limitStr != "" {
		limit, _ = strconv.Atoi(limitStr)
	}

	data, err := c.sensorService.GetSensorHistory(uint(deviceID), startTime, endTime, limit)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, data)
}

// GetLatestSensorData godoc
// @Summary 获取最新传感器数据
// @Description 获取指定设备的最新传感器数据
// @Tags 传感器数据
// @Security ApiKeyAuth
// @Produce json
// @Param device_id path int true "设备ID"
// @Param data_type query string false "数据类型过滤"
// @Success 200 {object} models.SensorData
// @Router /api/sensors/latest/{device_id} [get]
func (c *SensorController) GetLatest(ctx *gin.Context) {
	deviceID, _ := strconv.ParseUint(ctx.Param("device_id"), 10, 32)
	dataType := ctx.Query("data_type")

	data, err := c.sensorService.GetLatestSensorData(uint(deviceID), dataType)
	if err != nil {
		response.InternalServerError(ctx, err.Error())
		return
	}

	response.Success(ctx, data)
}

// GetRecentRainfall godoc
// @Summary 查询设备最近降雨量
// @Description 按设备查看最近一段时间（默认最近两小时）内的降雨量，雨量按每条记录相对上一条的增量求和
// @Tags 传感器数据
// @Security ApiKeyAuth
// @Produce json
// @Param device_id path int true "设备ID"
// @Param hours query number false "统计时长（小时，默认 2）" default(2)
// @Success 200 {object} services.RainfallSummary
// @Router /api/sensors/rainfall/{device_id} [get]
func (c *SensorController) GetRecentRainfall(ctx *gin.Context) {
	deviceID, err := strconv.ParseUint(ctx.Param("device_id"), 10, 32)
	if err != nil || deviceID == 0 {
		response.BadRequest(ctx, "Invalid device_id")
		return
	}

	hours := 2.0
	if hoursStr := ctx.Query("hours"); hoursStr != "" {
		hours, err = strconv.ParseFloat(hoursStr, 64)
		if err != nil || hours <= 0 {
			response.BadRequest(ctx, "Invalid hours")
			return
		}
	}

	summary, err := c.sensorService.GetRecentRainfall(uint(deviceID), hours)
	if err != nil {
		respondSensorError(ctx, err)
		return
	}

	response.Success(ctx, summary)
}
