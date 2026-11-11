package continuity

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelUsageIsAuthenticatedScopedAndAllowlisted(t *testing.T) {
	db := setupAccountAPIOutcomeTest(t)
	secret := strings.Repeat("s", 32)
	t.Setenv(ContinuityInternalAPISecretEnv, secret)
	for _, row := range []model.Log{
		{Id: 1, UserId: 1, TokenId: 10, Type: model.LogTypeConsume, ModelName: "product-model", CreatedAt: 100, Username: "PRIVATE_USER", Content: "PRIVATE_CONTENT", Other: `{"secret":"PRIVATE_KEY"}`},
		{Id: 2, UserId: 1, TokenId: 10, Type: model.LogTypeConsume, ModelName: "product-model", CreatedAt: 199},
		{Id: 3, UserId: 1, TokenId: 10, Type: model.LogTypeConsume, ModelName: "product-model", CreatedAt: 200},
		{Id: 4, UserId: 2, TokenId: 20, Type: model.LogTypeConsume, ModelName: "PRIVATE_OTHER_MODEL", CreatedAt: 150},
		{Id: 5, UserId: 1, TokenId: 10, Type: 1, ModelName: "PRIVATE_NON_USAGE", CreatedAt: 150},
	} {
		require.NoError(t, db.Create(&row).Error)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	(plugin{}).Mount(router)
	body := `{"schema_version":1,"from":100,"until":200,"token_ids":[10,10]}`
	call := func(auth string, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/internal/continuity/model-usage", strings.NewReader(payload))
		req.Header.Set("Authorization", auth)
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		return res
	}
	require.Equal(t, http.StatusUnauthorized, call("", body).Code)
	response := call("Bearer "+secret, body)
	require.Equal(t, http.StatusOK, response.Code)
	var out struct {
		Success bool `json:"success"`
		Data    struct {
			SchemaVersion int               `json:"schema_version"`
			Models        []modelUsageCount `json:"models"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &out))
	require.True(t, out.Success)
	require.Equal(t, []modelUsageCount{{ModelID: "product-model", Requests: 2}}, out.Data.Models)
	require.NotContains(t, response.Body.String(), "PRIVATE_")
	require.NotContains(t, response.Body.String(), "token_id")
	require.NotContains(t, response.Body.String(), "username")
	for _, bad := range []string{`{}`, `{"schema_version":1,"from":200,"until":100,"token_ids":[10]}`, `{"schema_version":1,"from":100,"until":200,"token_ids":[0]}`} {
		require.Equal(t, http.StatusBadRequest, call("Bearer "+secret, bad).Code)
	}
}
