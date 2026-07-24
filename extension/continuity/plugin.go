package continuity

import (
	"os"
	"strings"

	"github.com/QuantumNous/new-api/extension"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

type plugin struct{}

func (plugin) Name() string {
	return "continuity"
}

func (plugin) Enabled() bool {
	return strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv)) != ""
}

func (plugin) Mount(router *gin.Engine) {
	internalRoute := router.Group("/internal/continuity")
	internalRoute.Use(middleware.RouteTag("internal"), continuityInternalAuth())
	{
		internalRoute.GET("/capabilities", capabilitiesHandler)
		internalRoute.PATCH("/users/:id/group", updateManagedUserGroupHandler)
		internalRoute.POST("/token-groups/batch", updateManagedTokenGroupsHandler)
		internalRoute.GET("/routing-groups", listRoutingGroupsHandler)
	}
}

func init() {
	extension.Register(plugin{})
}
