package extension

import (
	"fmt"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Plugin is a source-level extension compiled into a NewAPI fork.
//
// Plugins register themselves during package initialization. The host owns the
// single router mount point, which keeps fork-specific routes out of the core
// router assembly.
type Plugin interface {
	Name() string
	Enabled() bool
	Mount(router *gin.Engine)
}

// RelayV1MiddlewareProvider is an optional, narrow data-plane seam for source
// extensions. The host evaluates it only for enabled plugins while assembling
// /v1 relay routes, so disabled extensions do not enter the request chain.
type RelayV1MiddlewareProvider interface {
	RelayV1Middleware() gin.HandlerFunc
}

// RelaySuccessEvent is the privacy-minimal outcome emitted after a relay has
// completed successfully. It intentionally contains no request, token, user,
// channel credential, or response data.
type RelaySuccessEvent struct {
	Group      string
	Model      string
	ObservedAt time.Time
	LatencyMs  int64
}

// RelayOutcomeEvent is the privacy-minimal final result of one logical relay
// request after retries. StatusRelevant is false for local validation/billing
// rejections that did not test the selected group/model's serving path.
type RelayOutcomeEvent struct {
	Group          string
	Model          string
	ObservedAt     time.Time
	LatencyMs      int64
	Success        bool
	StatusRelevant bool
}

// RelaySuccessObserver is an optional data-plane observation seam for source
// extensions. Observers must return quickly; notification happens inline with
// the successful relay completion path.
type RelaySuccessObserver interface {
	ObserveRelaySuccess(event RelaySuccessEvent)
}

// RelayOutcomeObserver receives both successful and failed final outcomes.
// Observers must return quickly and must not retain user request data.
type RelayOutcomeObserver interface {
	ObserveRelayOutcome(event RelayOutcomeEvent)
}

var pluginRegistry = struct {
	sync.RWMutex
	plugins map[string]Plugin
	order   []string
}{
	plugins: make(map[string]Plugin),
}

func Register(plugin Plugin) {
	if plugin == nil || plugin.Name() == "" {
		panic("extension: plugin name is required")
	}

	pluginRegistry.Lock()
	defer pluginRegistry.Unlock()
	name := plugin.Name()
	if _, exists := pluginRegistry.plugins[name]; exists {
		panic(fmt.Sprintf("extension: plugin %q is already registered", name))
	}
	pluginRegistry.plugins[name] = plugin
	pluginRegistry.order = append(pluginRegistry.order, name)
}

func MountAll(router *gin.Engine) {
	pluginRegistry.RLock()
	plugins := make([]Plugin, 0, len(pluginRegistry.order))
	for _, name := range pluginRegistry.order {
		plugins = append(plugins, pluginRegistry.plugins[name])
	}
	pluginRegistry.RUnlock()

	for _, plugin := range plugins {
		if plugin.Enabled() {
			plugin.Mount(router)
		}
	}
}

// RelayV1Middlewares returns middleware contributed by currently enabled
// source extensions in deterministic registration order.
func RelayV1Middlewares() []gin.HandlerFunc {
	pluginRegistry.RLock()
	plugins := make([]Plugin, 0, len(pluginRegistry.order))
	for _, name := range pluginRegistry.order {
		plugins = append(plugins, pluginRegistry.plugins[name])
	}
	pluginRegistry.RUnlock()

	middlewares := make([]gin.HandlerFunc, 0)
	for _, plugin := range plugins {
		provider, ok := plugin.(RelayV1MiddlewareProvider)
		if !ok || !plugin.Enabled() {
			continue
		}
		if middleware := provider.RelayV1Middleware(); middleware != nil {
			middlewares = append(middlewares, middleware)
		}
	}
	return middlewares
}

// NotifyRelaySuccess sends one sanitized successful relay outcome to enabled
// source extensions in deterministic registration order.
func NotifyRelaySuccess(event RelaySuccessEvent) {
	NotifyRelayOutcome(RelayOutcomeEvent{
		Group:          event.Group,
		Model:          event.Model,
		ObservedAt:     event.ObservedAt,
		LatencyMs:      event.LatencyMs,
		Success:        true,
		StatusRelevant: true,
	})
}

// NotifyRelayOutcome sends one sanitized final relay outcome to enabled source
// extensions in deterministic registration order. Legacy success observers
// continue to receive successful outcomes only.
func NotifyRelayOutcome(event RelayOutcomeEvent) {
	pluginRegistry.RLock()
	plugins := make([]Plugin, 0, len(pluginRegistry.order))
	for _, name := range pluginRegistry.order {
		plugins = append(plugins, pluginRegistry.plugins[name])
	}
	pluginRegistry.RUnlock()

	for _, plugin := range plugins {
		if !plugin.Enabled() {
			continue
		}
		if observer, ok := plugin.(RelayOutcomeObserver); ok {
			notifyRelayOutcomeObserver(observer, event)
		}
		if event.Success {
			if observer, ok := plugin.(RelaySuccessObserver); ok {
				notifyRelaySuccessObserver(observer, RelaySuccessEvent{
					Group:      event.Group,
					Model:      event.Model,
					ObservedAt: event.ObservedAt,
					LatencyMs:  event.LatencyMs,
				})
			}
		}
	}
}

func notifyRelayOutcomeObserver(observer RelayOutcomeObserver, event RelayOutcomeEvent) {
	defer func() {
		_ = recover()
	}()
	observer.ObserveRelayOutcome(event)
}

func notifyRelaySuccessObserver(observer RelaySuccessObserver, event RelaySuccessEvent) {
	defer func() {
		_ = recover()
	}()
	observer.ObserveRelaySuccess(event)
}
