package continuity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginCanBeDisabledWithoutMountingRoutes(t *testing.T) {
	t.Setenv(ContinuityInternalAPISecretEnv, "")
	assert.False(t, (plugin{}).Enabled())
}

func TestCapabilitiesExposeVersionedHostContract(t *testing.T) {
	secret := strings.Repeat("c", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	request := httptest.NewRequest(http.MethodGet, "/internal/continuity/capabilities", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			ProtocolVersion int      `json:"protocol_version"`
			CacheCoherency  string   `json:"cache_coherency"`
			Capabilities    []string `json:"capabilities"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	assert.True(t, body.Success)
	assert.Equal(t, 1, body.Data.ProtocolVersion)
	assert.Equal(t, "single_process_population_fence_v1", body.Data.CacheCoherency)
	assert.Equal(t, []string{
		"group_model_status.checks.read",
		"group_model_status.checks.write",
		"group_model_status.read",
		"routing_groups.read",
		"token_groups.batch_write",
		"user_group.write",
	}, body.Data.Capabilities)
}
