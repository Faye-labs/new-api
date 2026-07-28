package continuity

import (
	"os"
	"strings"

	"github.com/QuantumNous/new-api/extension"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

type plugin struct{}

const ContinuityAccountAPIEnabledEnv = "CONTINUITY_ACCOUNT_API_ENABLED"

func accountAPIDataPlaneEnabled() bool {
	return os.Getenv(ContinuityAccountAPIEnabledEnv) == "1" &&
		validContinuityInternalSecret(strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv)))
}

func (plugin) Name() string {
	return "continuity"
}

func (plugin) Enabled() bool {
	return strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv)) != ""
}

func (plugin) RelayV1Middleware() gin.HandlerFunc {
	if !accountAPIDataPlaneEnabled() {
		return nil
	}
	return AccountAPIRequestFinality()
}

func (plugin) Mount(router *gin.Engine) {
	internalRoute := router.Group("/internal/continuity")
	internalRoute.Use(middleware.RouteTag("internal"), continuityInternalAuth())
	{
		internalRoute.GET("/capabilities", capabilitiesHandler)
		internalRoute.GET(
			"/account-api/users/:userId/tokens/:tokenId/requests/:requestId/outcome",
			accountAPIRequestOutcomeHandler,
		)
		internalRoute.POST("/account-api/tokens/:id/disable", disableAccountAPITokenHandler)
		internalRoute.PATCH("/users/:id/group", updateManagedUserGroupHandler)
		internalRoute.POST("/token-groups/batch", updateManagedTokenGroupsHandler)
		internalRoute.GET("/routing-groups", listRoutingGroupsHandler)
		internalRoute.GET("/group-model-status", groupModelStatusHandler)
		internalRoute.GET("/group-model-status/checks", groupModelStatusChecksHandler)
		internalRoute.POST("/group-model-status/checks", startGroupModelStatusCheckHandler)
		internalRoute.GET("/group-model-status/exclusions", getGroupModelProbeExclusionsHandler)
		internalRoute.PUT("/group-model-status/exclusions", replaceGroupModelProbeExclusionsHandler)
	}
}

func init() {
	middleware.RegisterTokenAuthGuard("continuity-account-api", accountAPITokenAuthGuard)
	extension.Register(plugin{})
	service.RegisterSystemTaskHandler(continuityGroupModelProbeHandler{})
}
