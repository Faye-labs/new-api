package extension

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

type testPlugin struct {
	name    string
	enabled bool
	path    string
}

type testRelayPlugin struct {
	testPlugin
	headerValue string
}

type testRelaySuccessPlugin struct {
	testPlugin
	events *[]RelaySuccessEvent
}

type testPanickingRelaySuccessPlugin struct {
	testPlugin
}

func (testPanickingRelaySuccessPlugin) ObserveRelaySuccess(RelaySuccessEvent) {
	panic("observer failure")
}

func (plugin testRelaySuccessPlugin) ObserveRelaySuccess(event RelaySuccessEvent) {
	*plugin.events = append(*plugin.events, event)
}

func (plugin testRelayPlugin) RelayV1Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Test-Relay-Plugin", plugin.headerValue)
		c.Next()
	}
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

func TestRelayV1MiddlewaresIncludeOnlyEnabledProviders(t *testing.T) {
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

	Register(testPlugin{name: "enabled-without-provider", enabled: true})
	Register(testRelayPlugin{
		testPlugin:  testPlugin{name: "disabled-provider"},
		headerValue: "disabled",
	})
	Register(testRelayPlugin{
		testPlugin:  testPlugin{name: "enabled-provider", enabled: true},
		headerValue: "enabled",
	})

	middlewares := RelayV1Middlewares()
	assert.Len(t, middlewares, 1)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/relay", middlewares[0], func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/relay", nil))
	assert.Equal(t, http.StatusNoContent, response.Code)
	assert.Equal(t, "enabled", response.Header().Get("X-Test-Relay-Plugin"))
}

func TestNotifyRelaySuccessIncludesOnlyEnabledObservers(t *testing.T) {
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

	disabledEvents := make([]RelaySuccessEvent, 0)
	enabledEvents := make([]RelaySuccessEvent, 0)
	Register(testRelaySuccessPlugin{
		testPlugin: testPlugin{name: "disabled-observer"},
		events:     &disabledEvents,
	})
	Register(testPlugin{name: "enabled-without-observer", enabled: true})
	Register(testPanickingRelaySuccessPlugin{
		testPlugin: testPlugin{name: "panicking-observer", enabled: true},
	})
	Register(testRelaySuccessPlugin{
		testPlugin: testPlugin{name: "enabled-observer", enabled: true},
		events:     &enabledEvents,
	})

	event := RelaySuccessEvent{
		Group:      "standard",
		Model:      "model-a",
		ObservedAt: time.Date(2026, time.August, 4, 12, 0, 0, 0, time.UTC),
		LatencyMs:  125,
	}
	NotifyRelaySuccess(event)

	assert.Empty(t, disabledEvents)
	assert.Equal(t, []RelaySuccessEvent{event}, enabledEvents)
}
