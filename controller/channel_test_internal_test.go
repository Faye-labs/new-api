package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier: "base",
	})

	require.Equal(t, "tiered_expr", other["billing_mode"])
	require.Equal(t, "base", other["matched_tier"])
	require.NotEmpty(t, other["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestProbeChannelTreatsMissingProbeIdentityAsIndeterminate(t *testing.T) {
	setupModelListControllerTestDB(t)

	result := ProbeChannel(
		nil,
		&model.Channel{
			Id:     1,
			Type:   constant.ChannelTypeOpenAI,
			Status: common.ChannelStatusEnabled,
		},
		"gpt-4o-mini",
		"standard",
	)

	require.Equal(t, ChannelProbeStatusIndeterminate, result.Status)
}

func TestResolveChannelProbeEndpointRejectsUncertainFamilies(t *testing.T) {
	tests := []struct {
		name         string
		channelType  int
		modelName    string
		expectedType constant.EndpointType
		supported    bool
	}{
		{
			name:         "chat",
			channelType:  constant.ChannelTypeOpenAI,
			modelName:    "gpt-4o-mini",
			expectedType: constant.EndpointTypeOpenAI,
			supported:    true,
		},
		{
			name:         "codex channel",
			channelType:  constant.ChannelTypeCodex,
			modelName:    "gpt-5",
			expectedType: constant.EndpointTypeOpenAIResponse,
			supported:    true,
		},
		{
			name:         "known responses-only model",
			channelType:  constant.ChannelTypeOpenAI,
			modelName:    "o3-pro",
			expectedType: constant.EndpointTypeOpenAIResponse,
			supported:    true,
		},
		{
			name:         "compact model",
			channelType:  constant.ChannelTypeOpenAI,
			modelName:    "gpt-5" + ratio_setting.CompactModelSuffix,
			expectedType: constant.EndpointTypeOpenAIResponseCompact,
			supported:    true,
		},
		{
			name:         "embedding model",
			channelType:  constant.ChannelTypeOpenAI,
			modelName:    "text-embedding-3-small",
			expectedType: constant.EndpointTypeEmbeddings,
			supported:    true,
		},
		{
			name:         "rerank channel",
			channelType:  constant.ChannelTypeJina,
			modelName:    "jina-reranker-v2-base-multilingual",
			expectedType: constant.EndpointTypeJinaRerank,
			supported:    true,
		},
		{
			name:        "image model",
			channelType: constant.ChannelTypeOpenAI,
			modelName:   "gpt-image-1",
			supported:   false,
		},
		{
			name:        "audio model",
			channelType: constant.ChannelTypeOpenAI,
			modelName:   "whisper-1",
			supported:   false,
		},
		{
			name:        "moderation model",
			channelType: constant.ChannelTypeOpenAI,
			modelName:   "omni-moderation-latest",
			supported:   false,
		},
		{
			name:        "advanced custom channel",
			channelType: constant.ChannelTypeAdvancedCustom,
			modelName:   "gpt-4o-mini",
			supported:   false,
		},
		{
			name:        "unknown channel",
			channelType: constant.ChannelTypeUnknown,
			modelName:   "gpt-4o-mini",
			supported:   false,
		},
		{
			name:        "opaque model family",
			channelType: constant.ChannelTypeOpenAI,
			modelName:   "model-a",
			supported:   false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpointType, supported := resolveChannelProbeEndpoint(
				&model.Channel{Type: test.channelType},
				test.modelName,
			)
			require.Equal(t, test.supported, supported)
			require.Equal(t, string(test.expectedType), endpointType)
		})
	}
}

func TestProbeChannelUsesAdaptorWithoutConsumeLogOrQuotaMutation(t *testing.T) {
	database := setupModelListControllerTestDB(t)
	require.NoError(t, database.AutoMigrate(&model.Log{}))
	service.InitHttpClient()
	require.False(t, common.MemoryCacheEnabled)
	rootUser := model.User{
		Id:       1,
		Username: "root-probe",
		Password: "password",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Quota:    1000,
		Group:    "default",
	}
	require.NoError(t, database.Create(&rootUser).Error)

	requestPath := make(chan string, 1)
	responseWriteError := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestPath <- request.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(`{
			"id":"chatcmpl-probe",
			"object":"chat.completion",
			"created":1721820000,
			"model":"gpt-4o-mini",
			"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
		responseWriteError <- err
	}))
	defer upstream.Close()

	baseURL := upstream.URL
	channel := &model.Channel{
		Id:           10,
		Type:         constant.ChannelTypeOpenAI,
		Key:          "test-key-one\ntest-key-two",
		Status:       common.ChannelStatusEnabled,
		Name:         "probe-channel",
		BaseURL:      &baseURL,
		Models:       "gpt-4o-mini",
		Group:        "standard",
		ResponseTime: 777,
		ChannelInfo: model.ChannelInfo{
			IsMultiKey:           true,
			MultiKeySize:         2,
			MultiKeyPollingIndex: 1,
			MultiKeyMode:         constant.MultiKeyModePolling,
		},
	}
	require.NoError(t, database.Create(channel).Error)
	channelInfoBefore := channel.ChannelInfo

	result := ProbeChannel(nil, channel, "gpt-4o-mini", "standard")
	require.Equal(t, ChannelProbeStatusSucceeded, result.Status)
	require.Equal(t, "/v1/chat/completions", <-requestPath)
	require.NoError(t, <-responseWriteError)

	var consumeLogCount int64
	require.NoError(t, database.Model(&model.Log{}).
		Where("type = ?", model.LogTypeConsume).
		Count(&consumeLogCount).Error)
	require.Zero(t, consumeLogCount)

	var storedUser model.User
	require.NoError(t, database.First(&storedUser, rootUser.Id).Error)
	require.Equal(t, 1000, storedUser.Quota)
	require.Equal(t, 777, channel.ResponseTime)
	require.Equal(t, channelInfoBefore, channel.ChannelInfo)

	var storedChannel model.Channel
	require.NoError(t, database.First(&storedChannel, channel.Id).Error)
	require.Equal(t, channelInfoBefore, storedChannel.ChannelInfo)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "已有通道测试任务正在运行或等待中")
}
