package middleware

import (
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// TokenAuthGuard is a source-extension hook that runs only after the ordinary
// token has been validated. Returning false means the guard has written and
// aborted the response.
type TokenAuthGuard func(c *gin.Context, token *model.Token) bool

var tokenAuthGuards = struct {
	sync.RWMutex
	guards map[string]TokenAuthGuard
	order  []string
}{
	guards: make(map[string]TokenAuthGuard),
}

// RegisterTokenAuthGuard registers one process-wide guard in deterministic
// order. Names are unique so two source extensions cannot silently replace
// each other's security policy.
func RegisterTokenAuthGuard(name string, guard TokenAuthGuard) {
	if name == "" || guard == nil {
		panic("middleware: token auth guard name and function are required")
	}
	tokenAuthGuards.Lock()
	defer tokenAuthGuards.Unlock()
	if _, exists := tokenAuthGuards.guards[name]; exists {
		panic(fmt.Sprintf("middleware: token auth guard %q is already registered", name))
	}
	tokenAuthGuards.guards[name] = guard
	tokenAuthGuards.order = append(tokenAuthGuards.order, name)
}

func runTokenAuthGuards(c *gin.Context, token *model.Token) bool {
	tokenAuthGuards.RLock()
	defer tokenAuthGuards.RUnlock()
	for _, name := range tokenAuthGuards.order {
		if !tokenAuthGuards.guards[name](c, token) {
			return false
		}
	}
	return true
}
