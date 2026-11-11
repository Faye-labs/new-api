package continuity

import (
	"net/http"
	"sort"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
	// Short read batches release SQLite rollback-journal locks between queries.
	// Deduplicate before batching so repeated tokens cannot double-count requests.
	ids := make([]int, 0, len(request.TokenIDs))
	seen := make(map[int]bool)
	for _, id := range request.TokenIDs {
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	counts := make(map[string]int64)
	for start := 0; start < len(ids); start += 32 {
		end := min(start+32, len(ids))
		var batch []modelUsageCount
		err := model.LOG_DB.WithContext(c.Request.Context()).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)}).
			Model(&model.Log{}).Select("model_name AS model_id, COUNT(*) AS requests").
			Where("type = ? AND token_id IN ? AND created_at >= ? AND created_at < ?", model.LogTypeConsume, ids[start:end], request.From, request.Until).
			Group("model_name").Find(&batch).Error
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "model_usage_unavailable"})
			return
		}
		for _, row := range batch {
			counts[row.ModelID] += row.Requests
		}
	}
	rows := make([]modelUsageCount, 0, len(counts))
	for id, count := range counts {
		rows = append(rows, modelUsageCount{ModelID: id, Requests: count})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ModelID < rows[j].ModelID })
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"schema_version": 1, "models": rows}})
}
