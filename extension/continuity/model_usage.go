package continuity

import (
	"net/http"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type modelUsageRequest struct {
	SchemaVersion int   `json:"schema_version"`
	From          int64 `json:"from"`
	Until         int64 `json:"until"`
	TokenIDs      []int `json:"token_ids"`
}
type modelUsageCount struct {
	ModelID  string `json:"model_id"`
	Requests int64  `json:"requests"`
}

// The trusted caller owns account classification; this boundary owns raw request logs.
// No account, channel, pricing, credential, content or infrastructure fields are returned.
func modelUsageHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var request modelUsageRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil || request.SchemaVersion != 1 || request.From < 0 || request.Until <= request.From || len(request.TokenIDs) == 0 || len(request.TokenIDs) > 1000 {
		c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "invalid_model_usage_request"})
		return
	}
	for _, id := range request.TokenIDs {
		if id <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "invalid_model_usage_request"})
			return
		}
	}
	rows := make([]modelUsageCount, 0)
	err := model.LOG_DB.Model(&model.Log{}).Select("model_name AS model_id, COUNT(*) AS requests").
		Where("type = ? AND token_id IN ? AND created_at >= ? AND created_at < ?", model.LogTypeConsume, request.TokenIDs, request.From, request.Until).
		Group("model_name").Order("model_name ASC").Find(&rows).Error
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "model_usage_unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"schema_version": 1, "models": rows}})
}
