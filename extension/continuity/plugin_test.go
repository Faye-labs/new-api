package continuity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

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
		"group_model_status.exclusions.read",
		"group_model_status.exclusions.write",
		"group_model_status.read",
		"routing_groups.read",
		"token_groups.batch_write",
		"user_group.write",
	}, body.Data.Capabilities)
}

func TestProbeExclusionEndpointRequiresExplicitReadinessInitialization(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	secret := strings.Repeat("r", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	getRequest := httptest.NewRequest(
		http.MethodGet,
		"/internal/continuity/group-model-status/exclusions",
		nil,
	)
	getRequest.Header.Set("Authorization", "Bearer "+secret)
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, getRequest)
	require.Equal(t, http.StatusOK, getResponse.Code)

	var before struct {
		Success bool `json:"success"`
		Data    struct {
			Initialized bool                                 `json:"initialized"`
			Pairs       []continuityGroupModelProbeExclusion `json:"pairs"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(getResponse.Body.Bytes(), &before))
	assert.True(t, before.Success)
	assert.False(t, before.Data.Initialized)
	assert.Empty(t, before.Data.Pairs)

	putRequest := httptest.NewRequest(
		http.MethodPut,
		"/internal/continuity/group-model-status/exclusions",
		strings.NewReader(`{"pairs":[]}`),
	)
	putRequest.Header.Set("Authorization", "Bearer "+secret)
	putRequest.Header.Set("Content-Type", "application/json")
	putResponse := httptest.NewRecorder()
	router.ServeHTTP(putResponse, putRequest)
	require.Equal(t, http.StatusOK, putResponse.Code)

	afterGetRequest := httptest.NewRequest(
		http.MethodGet,
		"/internal/continuity/group-model-status/exclusions",
		nil,
	)
	afterGetRequest.Header.Set("Authorization", "Bearer "+secret)
	afterGetResponse := httptest.NewRecorder()
	router.ServeHTTP(afterGetResponse, afterGetRequest)
	require.Equal(t, http.StatusOK, afterGetResponse.Code)

	var after struct {
		Success bool `json:"success"`
		Data    struct {
			Initialized bool                                 `json:"initialized"`
			Pairs       []continuityGroupModelProbeExclusion `json:"pairs"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(afterGetResponse.Body.Bytes(), &after))
	assert.True(t, after.Success)
	assert.True(t, after.Data.Initialized)
	assert.Empty(t, after.Data.Pairs)
}

func TestProbeExclusionEndpointsAreSecretAuthenticatedAndRoundTrip(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"standard":1}`))
	secret := strings.Repeat("e", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	request := httptest.NewRequest(
		http.MethodPut,
		"/internal/continuity/group-model-status/exclusions",
		strings.NewReader(`{"pairs":[{"group_key":"standard","model_id":"compat-model"}]}`),
	)
	request.Header.Set("Authorization", "Bearer "+secret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)

	getRequest := httptest.NewRequest(
		http.MethodGet,
		"/internal/continuity/group-model-status/exclusions",
		nil,
	)
	getRequest.Header.Set("Authorization", "Bearer "+secret)
	getResponse := httptest.NewRecorder()
	router.ServeHTTP(getResponse, getRequest)
	require.Equal(t, http.StatusOK, getResponse.Code)
	assert.Equal(t, "no-store", getResponse.Header().Get("Cache-Control"))

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			SchemaVersion int                                  `json:"schema_version"`
			Initialized   bool                                 `json:"initialized"`
			Pairs         []continuityGroupModelProbeExclusion `json:"pairs"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(getResponse.Body.Bytes(), &body))
	assert.True(t, body.Success)
	assert.Equal(t, continuityGroupModelProbeExclusionsSchema, body.Data.SchemaVersion)
	assert.True(t, body.Data.Initialized)
	assert.Equal(t, []continuityGroupModelProbeExclusion{{
		GroupKey: "standard",
		ModelID:  "compat-model",
	}}, body.Data.Pairs)

	for _, invalidBody := range []string{
		`{}`,
		`{"pairs":null}`,
	} {
		invalidRequest := httptest.NewRequest(
			http.MethodPut,
			"/internal/continuity/group-model-status/exclusions",
			strings.NewReader(invalidBody),
		)
		invalidRequest.Header.Set("Authorization", "Bearer "+secret)
		invalidRequest.Header.Set("Content-Type", "application/json")
		invalidResponse := httptest.NewRecorder()
		router.ServeHTTP(invalidResponse, invalidRequest)
		require.Equal(t, http.StatusBadRequest, invalidResponse.Code)
	}
	persisted, err := loadContinuityGroupModelProbeExclusions()
	require.NoError(t, err)
	assert.Equal(t, body.Data.Pairs, persisted)

	clearRequest := httptest.NewRequest(
		http.MethodPut,
		"/internal/continuity/group-model-status/exclusions",
		strings.NewReader(`{"pairs":[]}`),
	)
	clearRequest.Header.Set("Authorization", "Bearer "+secret)
	clearRequest.Header.Set("Content-Type", "application/json")
	clearResponse := httptest.NewRecorder()
	router.ServeHTTP(clearResponse, clearRequest)
	require.Equal(t, http.StatusOK, clearResponse.Code)

	pairs, err := loadContinuityGroupModelProbeExclusions()
	require.NoError(t, err)
	assert.Empty(t, pairs)
}
