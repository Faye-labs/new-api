package middleware

import (
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
)

// CaptureContinuityCacheEvidence stores valid Continuity cache evidence for
// logging and strips the private header before relay distribution begins.
func CaptureContinuityCacheEvidence() gin.HandlerFunc {
	return func(c *gin.Context) {
		service.CaptureContinuityCacheEvidence(c)
		c.Next()
	}
}
