package extension

import (
	"fmt"
	"sync"

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
