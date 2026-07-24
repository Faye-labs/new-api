package extension

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type testPlugin struct {
	name    string
	enabled bool
	path    string
}

func (plugin testPlugin) Name() string {
	return plugin.name
}

func (plugin testPlugin) Enabled() bool {
	return plugin.enabled
}

func (plugin testPlugin) Mount(router *gin.Engine) {
	router.GET(plugin.path, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
}

func TestMountAllSkipsDisabledPlugins(t *testing.T) {
	pluginRegistry.Lock()
	originalPlugins := pluginRegistry.plugins
	originalOrder := pluginRegistry.order
	pluginRegistry.plugins = make(map[string]Plugin)
	pluginRegistry.order = nil
	pluginRegistry.Unlock()
	t.Cleanup(func() {
		pluginRegistry.Lock()
		pluginRegistry.plugins = originalPlugins
		pluginRegistry.order = originalOrder
		pluginRegistry.Unlock()
	})

	Register(testPlugin{name: "disabled", path: "/disabled"})
	Register(testPlugin{name: "enabled", enabled: true, path: "/enabled"})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	MountAll(router)

	disabledResponse := httptest.NewRecorder()
	router.ServeHTTP(disabledResponse, httptest.NewRequest(http.MethodGet, "/disabled", nil))
	assert.Equal(t, http.StatusNotFound, disabledResponse.Code)

	enabledResponse := httptest.NewRecorder()
	router.ServeHTTP(enabledResponse, httptest.NewRequest(http.MethodGet, "/enabled", nil))
	assert.Equal(t, http.StatusNoContent, enabledResponse.Code)
}
