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
	// Bound each read by log IDs, not token count: one token can own years of logs.
	// Do not hold a transaction across pages; live billing writes can proceed between them.
	selected := make(map[int]bool, len(request.TokenIDs))
	for _, id := range request.TokenIDs {
		selected[id] = true
	}
	query := model.LOG_DB.WithContext(c.Request.Context()).Session(&gorm.Session{Logger: logger.Default.LogMode(logger.Silent)})
	var bounds struct {
		First int64
		Last  int64
	}
	err := query.Model(&model.Log{}).Select("COALESCE(MIN(id),0) AS first, COALESCE(MAX(id),0) AS last").
		Where("type = ? AND created_at >= ? AND created_at < ?", model.LogTypeConsume, request.From, request.Until).Scan(&bounds).Error
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "model_usage_unavailable"})
		return
	}
	counts := make(map[string]int64)
	for first := bounds.First; first > 0 && first <= bounds.Last; {
		var page []struct {
			ID        int64
			Type      int
			CreatedAt int64
			TokenID   int
			ModelName string
		}
		// Only the primary-key range participates in this query plan. Filtering below
		// prevents a secondary-index scan/sort from extending a page's read lock.
		err := query.Model(&model.Log{}).Select("id, type, created_at, token_id, model_name").
			Where("id >= ? AND id <= ?", first, bounds.Last).Order("id ASC").Limit(500).Find(&page).Error
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"success": false, "code": "model_usage_unavailable"})
			return
		}
		for _, row := range page {
			if row.Type == model.LogTypeConsume && selected[row.TokenID] && row.CreatedAt >= request.From && row.CreatedAt < request.Until {
				counts[row.ModelName]++
			}
		}
		if len(page) == 0 || page[len(page)-1].ID >= bounds.Last {
			break
		}
		first = page[len(page)-1].ID + 1
	}
	rows := make([]modelUsageCount, 0, len(counts))
	for id, count := range counts {
		rows = append(rows, modelUsageCount{ModelID: id, Requests: count})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ModelID < rows[j].ModelID })
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"schema_version": 1, "models": rows}})
}
