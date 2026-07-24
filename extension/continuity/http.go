package continuity

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

type continuityManagedUserGroupRequest struct {
	Group string `json:"group"`
}

type continuityManagedTokenGroupUpdateRequest struct {
	TokenID int    `json:"token_id"`
	UserID  int    `json:"user_id"`
	Group   string `json:"group"`
}

type continuityManagedTokenGroupsRequest struct {
	Updates []continuityManagedTokenGroupUpdateRequest `json:"updates"`
}

func capabilitiesHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"protocol_version":      1,
			"cache_coherency":       "single_process_population_fence_v1",
			"group_key_max_bytes":   continuityManagedGroupMaxLength,
			"token_group_batch_max": continuityManagedTokenBatchMax,
			"capabilities": []string{
				"routing_groups.read",
				"token_groups.batch_write",
				"user_group.write",
			},
		},
	})
}

func updateManagedUserGroupHandler(c *gin.Context) {
	userID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}

	var request continuityManagedUserGroupRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}

	group, changed, err := updateManagedUserGroup(userID, request.Group)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"user_id": userID,
			"group":   group,
			"changed": changed,
		},
	})
}

func updateManagedTokenGroupsHandler(c *gin.Context) {
	var request continuityManagedTokenGroupsRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}

	updates := make([]managedTokenGroupUpdate, 0, len(request.Updates))
	for _, update := range request.Updates {
		updates = append(updates, managedTokenGroupUpdate{
			TokenID: update.TokenID,
			UserID:  update.UserID,
			Group:   update.Group,
		})
	}
	results, err := updateManagedTokenGroups(updates)
	if err != nil {
		writeInternalError(c, err)
		return
	}

	updated := 0
	for _, result := range results {
		if result.Changed {
			updated++
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"updated":   updated,
			"unchanged": len(results) - updated,
			"tokens":    results,
		},
	})
}

func listRoutingGroupsHandler(c *gin.Context) {
	groups, err := listRoutingGroups()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"groups": groups,
		},
	})
}

func writeInternalError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "continuity_internal_error"
	message := "Continuity internal operation failed"

	switch {
	case errors.Is(err, errInvalidRequest):
		status = http.StatusBadRequest
		code = "invalid_request"
		message = "Invalid request"
	case errors.Is(err, errUnknownRoutingGroup):
		status = http.StatusBadRequest
		code = "unknown_routing_group"
		message = "Unknown routing group"
	case errors.Is(err, model.ErrContinuityManagedUserNotFound):
		status = http.StatusNotFound
		code = "user_not_found"
		message = "User not found"
	case errors.Is(err, model.ErrContinuityManagedTokenNotFound):
		status = http.StatusNotFound
		code = "token_not_found"
		message = "Token not found"
	case errors.Is(err, model.ErrContinuityManagedTokenDisabled):
		status = http.StatusConflict
		code = "token_not_enabled"
		message = "Token is not enabled"
	case errors.Is(err, model.ErrContinuityManagedTokenOwnerMismatch):
		status = http.StatusConflict
		code = "token_owner_mismatch"
		message = "Token owner does not match"
	default:
		common.SysLog(fmt.Sprintf("Continuity internal API error: %v", err))
	}

	c.JSON(status, gin.H{
		"success": false,
		"code":    code,
		"message": message,
	})
}
