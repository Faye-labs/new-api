package continuity

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/gin-gonic/gin"
)

const ContinuityInternalAPISecretEnv = "CONTINUITY_INTERNAL_API_SECRET"

func continuityInternalAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		expectedSecret := strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv))
		secretIsValid := len(expectedSecret) >= 32
		for _, character := range expectedSecret {
			if unicode.IsControl(character) || unicode.IsSpace(character) {
				secretIsValid = false
				break
			}
		}
		if !secretIsValid {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
				"success": false,
				"code":    "continuity_internal_not_configured",
				"message": "Continuity internal API is not configured",
			})
			return
		}

		authorization := strings.TrimSpace(c.GetHeader("Authorization"))
		parts := strings.Fields(authorization)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Header("WWW-Authenticate", "Bearer")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"code":    "continuity_internal_unauthorized",
				"message": "Invalid internal API credentials",
			})
			return
		}

		expectedHash := sha256.Sum256([]byte(expectedSecret))
		providedHash := sha256.Sum256([]byte(parts[1]))
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"success": false,
				"code":    "continuity_internal_unauthorized",
				"message": "Invalid internal API credentials",
			})
			return
		}

		c.Next()
	}
}
