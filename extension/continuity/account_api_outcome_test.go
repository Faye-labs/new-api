package continuity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupAccountAPIOutcomeTest(t *testing.T) *gorm.DB {
	t.Helper()
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalRedisEnabled := common.RedisEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()

	common.RedisEnabled = false
	common.LogConsumeEnabled = true
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = database
	model.LOG_DB = database
	require.NoError(t, database.AutoMigrate(&model.Token{}, &model.Log{}))

	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.RedisEnabled = originalRedisEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		sqlDB, dbErr := database.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
	return database
}

func insertAccountAPIOutcomeToken(
	t *testing.T,
	database *gorm.DB,
	tokenID int,
	userID int,
	name string,
) model.Token {
	t.Helper()
	token := model.Token{
		Id:             tokenID,
		UserId:         userID,
		Key:            fmt.Sprintf("accountoutcomekey%d", tokenID),
		Name:           name,
		Status:         common.TokenStatusEnabled,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	require.NoError(t, database.Create(&token).Error)
	return token
}

func signAccountAPIOutcomeRequest(
	secret string,
	timestamp int64,
	requestID string,
	method string,
	path string,
	userID int,
	tokenID int,
) string {
	payload := accountAPIRequestCanonicalPayload(
		strconv.FormatInt(timestamp, 10),
		requestID,
		method,
		path,
		userID,
		tokenID,
	)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payload))
	return accountAPIRequestBindingVersion + "=" + hex.EncodeToString(mac.Sum(nil))
}

func TestAccountAPIRequestBindingCanonicalVector(t *testing.T) {
	assert.Equal(
		t,
		"v1=80edfe018de8150c2b39a6e102243d09506dd313546760c53f98e98eb20c09cc",
		signAccountAPIOutcomeRequest(
			"0123456789abcdef0123456789abcdef",
			1722168000,
			"aareq_00000000000000000000000000000001",
			http.MethodPost,
			"/v1/chat/completions",
			70,
			700,
		),
	)
}

func addAccountAPIOutcomeRequestHeaders(
	request *http.Request,
	secret string,
	timestamp int64,
	requestID string,
	token model.Token,
) {
	request.Header.Set("Authorization", "Bearer sk-"+token.Key)
	request.Header.Set(AccountAPIRequestIDHeader, requestID)
	request.Header.Set(AccountAPIRequestTimestampHeader, strconv.FormatInt(timestamp, 10))
	request.Header.Set(
		AccountAPIRequestSignatureHeader,
		signAccountAPIOutcomeRequest(
			secret,
			timestamp,
			requestID,
			request.Method,
			request.URL.EscapedPath(),
			token.UserId,
			token.Id,
		),
	)
}

func TestAccountAPITrustedRequestPersistsExactLogAndFinalizedOutcome(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("f", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	token := insertAccountAPIOutcomeToken(
		t,
		database,
		501,
		71,
		"continuity-account-api-managed",
	)
	requestID := "account-request-final-001"
	path := "/v1/chat/completions"

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestId())
	router.POST(path, AccountAPIRequestFinality(), func(c *gin.Context) {
		assert.Empty(t, c.GetHeader(AccountAPIRequestIDHeader))
		assert.Empty(t, c.GetHeader(AccountAPIRequestTimestampHeader))
		assert.Empty(t, c.GetHeader(AccountAPIRequestSignatureHeader))
		assert.Equal(t, requestID, c.GetString(common.RequestIdKey))
		assert.Equal(t, requestID, c.Request.Context().Value(common.RequestIdKey))
		model.RecordConsumeLog(c, token.UserId, model.RecordConsumeLogParams{
			ChannelId:        8,
			PromptTokens:     17,
			CompletionTokens: 4,
			ModelName:        "external-model",
			TokenName:        token.Name,
			Quota:            250,
			TokenId:          token.Id,
			Group:            "external-route",
			Other:            map[string]interface{}{"source": "account-api"},
		})
		// A separate/ClickHouse log database may not make the just-written row
		// immediately queryable. Finality must use the synchronous success
		// collector rather than a read-after-write SELECT.
		model.LOG_DB = nil
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	now := time.Now().Unix()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"external-model"}`))
	addAccountAPIOutcomeRequestHeaders(request, secret, now, requestID, token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, requestID, response.Header().Get(common.RequestIdKey))
	var persisted model.Log
	require.NoError(t, database.Where(
		"user_id = ? AND token_id = ? AND request_id = ? AND type = ?",
		token.UserId,
		token.Id,
		requestID,
		model.LogTypeConsume,
	).First(&persisted).Error)
	assert.Equal(t, 250, persisted.Quota)

	outcome, err := getAccountAPIRequestOutcome(token.UserId, token.Id, requestID)
	require.NoError(t, err)
	assert.True(t, outcome.Finalized)
	assert.Equal(t, accountAPIRequestStateFinalized, outcome.State)
	require.NotNil(t, outcome.HTTPStatus)
	assert.Equal(t, http.StatusOK, *outcome.HTTPStatus)
	require.NotNil(t, outcome.FinalizedAt)
	require.Len(t, outcome.Logs, 1)
	assert.Equal(t, requestID, outcome.Logs[0].RequestID)
	assert.Equal(t, token.Id, outcome.Logs[0].TokenID)
	assert.Equal(t, model.LogTypeConsume, outcome.Logs[0].Type)
	assert.Equal(t, 17, outcome.Logs[0].PromptTokens)
	assert.Equal(t, 4, outcome.Logs[0].CompletionTokens)
	assert.Equal(t, 250, outcome.Logs[0].Quota)

	_, err = getAccountAPIRequestOutcome(token.UserId+1, token.Id, requestID)
	require.ErrorIs(t, err, errAccountAPIRequestOutcomeNotFound)
	_, err = getAccountAPIRequestOutcome(token.UserId, token.Id+1, requestID)
	require.ErrorIs(t, err, errAccountAPIRequestOutcomeNotFound)
	_, err = getAccountAPIRequestOutcome(token.UserId, token.Id, "account-request-other-001")
	require.ErrorIs(t, err, errAccountAPIRequestOutcomeNotFound)
}

func TestAccountAPICompletedErrorFinalizesWithoutConsumption(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("g", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	token := insertAccountAPIOutcomeToken(
		t,
		database,
		502,
		72,
		"continuity-account-api-managed",
	)
	requestID := "account-request-error-001"
	path := "/v1/chat/completions"

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(path, AccountAPIRequestFinality(), func(c *gin.Context) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model denied"})
	})
	request := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(request, secret, time.Now().Unix(), requestID, token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusBadRequest, response.Code)

	outcome, err := getAccountAPIRequestOutcome(token.UserId, token.Id, requestID)
	require.NoError(t, err)
	assert.True(t, outcome.Finalized)
	require.NotNil(t, outcome.HTTPStatus)
	assert.Equal(t, http.StatusBadRequest, *outcome.HTTPStatus)
	assert.Empty(t, outcome.Logs)
}

func TestAccountAPIOutcomeDistinguishesProcessingFromFinalizedZero(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	token := insertAccountAPIOutcomeToken(
		t,
		database,
		503,
		73,
		"continuity-account-api-managed",
	)
	requestID := "account-request-pending-001"
	_, err := beginAccountAPIRequestOutcome(&token, requestID)
	require.NoError(t, err)

	outcome, err := getAccountAPIRequestOutcome(token.UserId, token.Id, requestID)
	require.NoError(t, err)
	assert.False(t, outcome.Finalized)
	assert.Equal(t, accountAPIRequestStateProcessing, outcome.State)
	assert.Nil(t, outcome.HTTPStatus)
	assert.Nil(t, outcome.FinalizedAt)
	assert.Empty(t, outcome.Logs)
}

func TestAccountAPIOutcomeEndpointIsSecretAuthenticatedAndTupleBound(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("j", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	token := insertAccountAPIOutcomeToken(
		t,
		database,
		507,
		77,
		"continuity-account-api-managed",
	)
	requestID := "account-request-endpoint-001"
	_, err := beginAccountAPIRequestOutcome(&token, requestID)
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)
	path := fmt.Sprintf(
		"/internal/continuity/account-api/users/%d/tokens/%d/requests/%s/outcome",
		token.UserId,
		token.Id,
		requestID,
	)

	unauthorized := httptest.NewRequest(http.MethodGet, path, nil)
	unauthorizedResponse := httptest.NewRecorder()
	router.ServeHTTP(unauthorizedResponse, unauthorized)
	assert.Equal(t, http.StatusUnauthorized, unauthorizedResponse.Code)

	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "no-store", response.Header().Get("Cache-Control"))
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			UserID    int    `json:"user_id"`
			TokenID   int    `json:"token_id"`
			RequestID string `json:"request_id"`
			State     string `json:"state"`
			Finalized bool   `json:"finalized"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &body))
	assert.True(t, body.Success)
	assert.Equal(t, token.UserId, body.Data.UserID)
	assert.Equal(t, token.Id, body.Data.TokenID)
	assert.Equal(t, requestID, body.Data.RequestID)
	assert.Equal(t, accountAPIRequestStateProcessing, body.Data.State)
	assert.False(t, body.Data.Finalized)

	wrongOwner := httptest.NewRequest(
		http.MethodGet,
		strings.Replace(path, "/users/77/", "/users/78/", 1),
		nil,
	)
	wrongOwner.Header.Set("Authorization", "Bearer "+secret)
	wrongOwnerResponse := httptest.NewRecorder()
	router.ServeHTTP(wrongOwnerResponse, wrongOwner)
	assert.Equal(t, http.StatusNotFound, wrongOwnerResponse.Code)
}

func TestAccountAPIRequestBindingRejectsForgeryAndDuplicateUse(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("h", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	managed := insertAccountAPIOutcomeToken(
		t,
		database,
		504,
		74,
		"continuity-account-api-managed",
	)
	ordinary := insertAccountAPIOutcomeToken(t, database, 505, 75, "ordinary-user-token")
	path := "/v1/chat/completions"

	gin.SetMode(gin.TestMode)
	router := gin.New()
	downstreamCalls := 0
	router.POST(path, AccountAPIRequestFinality(), func(c *gin.Context) {
		downstreamCalls++
		c.Status(http.StatusNoContent)
	})

	forged := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(
		forged,
		secret,
		time.Now().Unix(),
		"account-request-forged-001",
		ordinary,
	)
	forgedResponse := httptest.NewRecorder()
	router.ServeHTTP(forgedResponse, forged)
	assert.Equal(t, http.StatusForbidden, forgedResponse.Code)
	assert.Equal(t, 0, downstreamCalls)
	assert.Empty(t, forged.Header.Get(AccountAPIRequestSignatureHeader))

	wrongSignature := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(
		wrongSignature,
		strings.Repeat("x", 32),
		time.Now().Unix(),
		"account-request-forged-002",
		managed,
	)
	wrongSignatureResponse := httptest.NewRecorder()
	router.ServeHTTP(wrongSignatureResponse, wrongSignature)
	assert.Equal(t, http.StatusForbidden, wrongSignatureResponse.Code)
	assert.Equal(t, 0, downstreamCalls)

	requestID := "account-request-once-001"
	timestamp := time.Now().Unix()
	first := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(first, secret, timestamp, requestID, managed)
	firstResponse := httptest.NewRecorder()
	router.ServeHTTP(firstResponse, first)
	assert.Equal(t, http.StatusNoContent, firstResponse.Code)
	assert.Equal(t, 1, downstreamCalls)

	duplicate := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(duplicate, secret, timestamp, requestID, managed)
	duplicateResponse := httptest.NewRecorder()
	router.ServeHTTP(duplicateResponse, duplicate)
	assert.Equal(t, http.StatusConflict, duplicateResponse.Code)
	assert.Equal(t, 1, downstreamCalls)
}

func TestAccountAPITokenAuthGuardAlwaysRequiresTrustedBindingForReservedToken(t *testing.T) {
	managed := &model.Token{Name: "continuity-account-api-managed"}
	ordinary := &model.Token{Name: "ordinary-user-token"}

	t.Setenv(ContinuityInternalAPISecretEnv, "")
	disabledResponse := httptest.NewRecorder()
	disabledContext, _ := gin.CreateTestContext(disabledResponse)
	assert.False(t, accountAPITokenAuthGuard(disabledContext, managed))
	assert.Equal(t, http.StatusUnauthorized, disabledResponse.Code)

	t.Setenv(ContinuityInternalAPISecretEnv, strings.Repeat("l", 32))
	ordinaryContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.True(t, accountAPITokenAuthGuard(ordinaryContext, ordinary))

	unboundResponse := httptest.NewRecorder()
	unboundContext, _ := gin.CreateTestContext(unboundResponse)
	assert.False(t, accountAPITokenAuthGuard(unboundContext, managed))
	assert.True(t, unboundContext.IsAborted())
	assert.Equal(t, http.StatusUnauthorized, unboundResponse.Code)

	boundContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	boundContext.Set(common.ContinuityAccountAPIRequestBoundKey, true)
	assert.True(t, accountAPITokenAuthGuard(boundContext, managed))
}

func TestAccountAPILogWriteFailureNeverProducesFalseFinality(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("i", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	token := insertAccountAPIOutcomeToken(
		t,
		database,
		506,
		76,
		"continuity-account-api-managed",
	)
	requestID := "account-request-log-fail-001"
	path := "/v1/chat/completions"

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(path, AccountAPIRequestFinality(), func(c *gin.Context) {
		require.NoError(t, database.Migrator().DropTable(&model.Log{}))
		model.RecordConsumeLog(c, token.UserId, model.RecordConsumeLogParams{
			ModelName: "external-model",
			TokenName: token.Name,
			Quota:     100,
			TokenId:   token.Id,
		})
		c.Status(http.StatusOK)
	})
	request := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(request, secret, time.Now().Unix(), requestID, token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)

	outcome, err := getAccountAPIRequestOutcome(token.UserId, token.Id, requestID)
	require.NoError(t, err)
	assert.False(t, outcome.Finalized)
	assert.Equal(t, accountAPIRequestStateIndeterminate, outcome.State)
	assert.Nil(t, outcome.HTTPStatus)
	require.NotNil(t, outcome.FinalizedAt)
	assert.Empty(t, outcome.Logs)
}

func TestAccountAPILogDisableRaceNeverProducesFalseFinality(t *testing.T) {
	database := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("k", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	token := insertAccountAPIOutcomeToken(
		t,
		database,
		508,
		78,
		"continuity-account-api-managed",
	)
	requestID := "account-request-log-toggle-001"
	path := "/v1/chat/completions"

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST(path, AccountAPIRequestFinality(), func(c *gin.Context) {
		common.LogConsumeEnabled = false
		model.RecordConsumeLog(c, token.UserId, model.RecordConsumeLogParams{
			ModelName: "external-model",
			TokenName: token.Name,
			Quota:     100,
			TokenId:   token.Id,
		})
		common.LogConsumeEnabled = true
		c.Status(http.StatusOK)
	})
	request := httptest.NewRequest(http.MethodPost, path, nil)
	addAccountAPIOutcomeRequestHeaders(request, secret, time.Now().Unix(), requestID, token)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code)

	outcome, err := getAccountAPIRequestOutcome(token.UserId, token.Id, requestID)
	require.NoError(t, err)
	assert.False(t, outcome.Finalized)
	assert.Equal(t, accountAPIRequestStateIndeterminate, outcome.State)
}

func TestAccountAPIRequestMiddlewareIsInertWithoutPrivateHeaders(t *testing.T) {
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	model.DB = nil
	model.LOG_DB = nil
	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
	})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestId())
	router.POST("/v1/chat/completions", AccountAPIRequestFinality(), func(c *gin.Context) {
		assert.NotEqual(t, "attacker-chosen-id", c.GetString(common.RequestIdKey))
		c.Status(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.Header.Set(common.RequestIdKey, "attacker-chosen-id")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)

	assert.Equal(t, http.StatusNoContent, response.Code)
	assert.NotEqual(t, "attacker-chosen-id", response.Header().Get(common.RequestIdKey))
}
