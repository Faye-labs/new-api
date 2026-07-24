package continuity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

const continuityInternalTestSecret = "correct-internal-secret-at-least-32-bytes"

func continuityInternalTestRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET(
		"/internal",
		continuityInternalAuth(),
		func(c *gin.Context) { c.Status(http.StatusNoContent) },
	)
	return router
}

func TestContinuityInternalAuthFailsClosedForInvalidConfiguration(t *testing.T) {
	for _, secret := range []string{
		"",
		"short-internal-secret",
		strings.Repeat("a", 31),
		"sixteen-characters\nsixteen-characters",
		"sixteen-characters sixteen-characters",
	} {
		t.Setenv(ContinuityInternalAPISecretEnv, secret)
		request := httptest.NewRequest(http.MethodGet, "/internal", nil)
		response := httptest.NewRecorder()

		continuityInternalTestRouter().ServeHTTP(response, request)

		assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	}
}

func TestContinuityInternalAuthRejectsMissingAndWrongBearer(t *testing.T) {
	t.Setenv(ContinuityInternalAPISecretEnv, continuityInternalTestSecret)
	for _, authorization := range []string{"", "Bearer wrong-secret", "Basic " + continuityInternalTestSecret} {
		request := httptest.NewRequest(http.MethodGet, "/internal", nil)
		request.Header.Set("Authorization", authorization)
		response := httptest.NewRecorder()

		continuityInternalTestRouter().ServeHTTP(response, request)

		assert.Equal(t, http.StatusUnauthorized, response.Code)
		assert.Equal(t, "Bearer", response.Header().Get("WWW-Authenticate"))
	}
}

func TestContinuityInternalAuthAcceptsExactBearerSecret(t *testing.T) {
	secret := strings.Repeat("a", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, "  "+secret+"  ")
	request := httptest.NewRequest(http.MethodGet, "/internal", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()

	continuityInternalTestRouter().ServeHTTP(response, request)

	assert.Equal(t, http.StatusNoContent, response.Code)
}
