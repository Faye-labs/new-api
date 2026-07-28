package continuity

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

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

type continuityAccountAPITokenDisableRequest struct {
	UserID int `json:"user_id"`
}

type continuityGroupModelProbeExclusionsRequest struct {
	Pairs *[]continuityGroupModelProbeExclusion `json:"pairs"`
}

func capabilitiesHandler(c *gin.Context) {
	capabilities := []string{
		"account_api_requests.finality.read",
	}
	if accountAPIDataPlaneEnabled() {
		capabilities = append(capabilities, "account_api_requests.trusted_binding.v1")
	}
	capabilities = append(capabilities,
		"account_api_tokens.disable",
		"group_model_status.checks.read",
		"group_model_status.checks.write",
		"group_model_status.exclusions.read",
		"group_model_status.exclusions.write",
		"group_model_status.read",
		"routing_groups.read",
		"token_groups.batch_write",
		"user_group.write",
	)
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"protocol_version":      1,
			"cache_coherency":       "single_process_population_fence_v1",
			"group_key_max_bytes":   continuityManagedGroupMaxLength,
			"token_group_batch_max": continuityManagedTokenBatchMax,
			"capabilities":          capabilities,
		},
	})
}

func accountAPIRequestOutcomeHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	userID, err := strconv.Atoi(c.Param("userId"))
	if err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}
	tokenID, err := strconv.Atoi(c.Param("tokenId"))
	if err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}
	outcome, err := getAccountAPIRequestOutcome(
		userID,
		tokenID,
		c.Param("requestId"),
	)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    outcome,
	})
}

func disableAccountAPITokenHandler(c *gin.Context) {
	tokenID, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}

	var request continuityAccountAPITokenDisableRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}
	result, err := disableAccountAPIToken(request.UserID, tokenID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    result,
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

func groupModelStatusHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	snapshot, err := groupModelStatusSnapshot(time.Now())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    snapshot,
	})
}

func groupModelStatusChecksHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	task, err := currentOrLatestContinuityGroupModelProbeTask()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if task == nil {
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data":    nil,
		})
		return
	}
	view, err := buildContinuityGroupModelProbeTaskView(task)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    view,
	})
}

func startGroupModelStatusCheckHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	task, created, err := enqueueContinuityGroupModelProbe()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	view, err := buildContinuityGroupModelProbeTaskView(task)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	view.Created = &created
	c.JSON(http.StatusAccepted, gin.H{
		"success": true,
		"message": "",
		"data":    view,
	})
}

func getGroupModelProbeExclusionsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	state, err := loadContinuityGroupModelProbeExclusionState()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"schema_version": continuityGroupModelProbeExclusionsSchema,
			"initialized":    state.Initialized,
			"pairs":          state.Pairs,
		},
	})
}

func replaceGroupModelProbeExclusionsHandler(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request continuityGroupModelProbeExclusionsRequest
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		writeInternalError(c, errInvalidRequest)
		return
	}
	if request.Pairs == nil {
		writeInternalError(c, errInvalidRequest)
		return
	}
	pairs, err := replaceContinuityGroupModelProbeExclusions(*request.Pairs)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"schema_version": continuityGroupModelProbeExclusionsSchema,
			"initialized":    true,
			"pairs":          pairs,
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
	case errors.Is(err, model.ErrContinuityManagedTokenIdentityMismatch):
		status = http.StatusConflict
		code = "token_identity_mismatch"
		message = "Token identity does not match"
	case errors.Is(err, errAccountAPIRequestOutcomeNotFound):
		status = http.StatusNotFound
		code = "request_outcome_not_found"
		message = "Account API request outcome was not found"
	default:
		common.SysLog(fmt.Sprintf("Continuity internal API error: %v", err))
	}

	c.JSON(status, gin.H{
		"success": false,
		"code":    code,
		"message": message,
	})
}
