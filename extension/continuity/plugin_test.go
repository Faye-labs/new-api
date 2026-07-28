package continuity

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginCanBeDisabledWithoutMountingRoutes(t *testing.T) {
	t.Setenv(ContinuityInternalAPISecretEnv, "")
	assert.False(t, (plugin{}).Enabled())
}

func TestAccountAPIDataPlaneMiddlewareDefaultsOffIndependently(t *testing.T) {
	secret := strings.Repeat("p", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	t.Setenv(ContinuityAccountAPIEnabledEnv, "")
	assert.Nil(t, (plugin{}).RelayV1Middleware())

	t.Setenv(ContinuityAccountAPIEnabledEnv, "1")
	assert.NotNil(t, (plugin{}).RelayV1Middleware())

	t.Setenv(ContinuityInternalAPISecretEnv, "short")
	assert.Nil(t, (plugin{}).RelayV1Middleware())
}

func TestCapabilitiesExposeVersionedHostContract(t *testing.T) {
	secret := strings.Repeat("c", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	t.Setenv(ContinuityAccountAPIEnabledEnv, "1")
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
		"account_api_requests.finality.read",
		"account_api_requests.trusted_binding.v1",
		"account_api_tokens.disable",
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

func TestCapabilitiesDoNotAdvertiseTrustedBindingWhileDataPlaneIsOff(t *testing.T) {
	secret := strings.Repeat("q", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	t.Setenv(ContinuityAccountAPIEnabledEnv, "")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	request := httptest.NewRequest(http.MethodGet, "/internal/continuity/capabilities", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	var body struct {
		Data struct {
			Capabilities []string `json:"capabilities"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	assert.NotContains(t, body.Data.Capabilities, "account_api_requests.trusted_binding.v1")
	assert.Contains(t, body.Data.Capabilities, "account_api_requests.finality.read")
}

func TestAccountAPITokenDisableEndpointIsOwnerBoundAndIdempotent(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, database.Create(&model.Token{
		Id:     701,
		UserId: 91,
		Key:    "hidden-account-api-key",
		Name:   "continuity-account-api-managed",
		Status: common.TokenStatusEnabled,
	}).Error)
	secret := strings.Repeat("d", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)

	disable := func(userID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(
			http.MethodPost,
			"/internal/continuity/account-api/tokens/701/disable",
			strings.NewReader(`{"user_id":`+userID+`}`),
		)
		request.Header.Set("Authorization", "Bearer "+secret)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		return response
	}

	wrongOwner := disable("92")
	require.Equal(t, http.StatusConflict, wrongOwner.Code)
	var stillActive model.Token
	require.NoError(t, database.First(&stillActive, 701).Error)
	assert.Equal(t, common.TokenStatusEnabled, stillActive.Status)

	first := disable("91")
	require.Equal(t, http.StatusOK, first.Code)
	assert.Equal(t, "no-store", first.Header().Get("Cache-Control"))
	var firstBody struct {
		Success bool `json:"success"`
		Data    struct {
			TokenID int  `json:"token_id"`
			UserID  int  `json:"user_id"`
			Changed bool `json:"changed"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(first.Body.Bytes(), &firstBody))
	assert.True(t, firstBody.Success)
	assert.Equal(t, 701, firstBody.Data.TokenID)
	assert.Equal(t, 91, firstBody.Data.UserID)
	assert.True(t, firstBody.Data.Changed)

	second := disable("91")
	require.Equal(t, http.StatusOK, second.Code)
	var secondBody struct {
		Data struct {
			Changed bool `json:"changed"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(second.Body.Bytes(), &secondBody))
	assert.False(t, secondBody.Data.Changed)

	var disabled model.Token
	require.NoError(t, database.Unscoped().First(&disabled, 701).Error)
	assert.Equal(t, common.TokenStatusDisabled, disabled.Status)
	assert.True(t, disabled.DeletedAt.Valid)
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
