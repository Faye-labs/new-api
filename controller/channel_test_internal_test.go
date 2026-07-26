package controller

import (
	"io"
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
	"github.com/QuantumNous/new-api/setting/model_setting"
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
			expectedType: constant.EndpointTypeOpenAI,
			supported:    true,
		},
		{
			name:         "known responses-only model",
			channelType:  constant.ChannelTypeOpenAI,
			modelName:    "o3-pro",
			expectedType: constant.EndpointTypeOpenAI,
			supported:    true,
		},
		{
			name:         "claude uses native messages",
			channelType:  constant.ChannelTypeAnthropic,
			modelName:    "claude-sonnet-4-6",
			expectedType: constant.EndpointTypeAnthropic,
			supported:    true,
		},
		{
			name:         "seed text model",
			channelType:  constant.ChannelTypeOpenAI,
			modelName:    "seed-1-6-thinking-250715",
			expectedType: constant.EndpointTypeOpenAI,
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
			name:        "seedance video model",
			channelType: constant.ChannelTypeVolcEngine,
			modelName:   "seedance-1-0-pro-250528",
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
	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })
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
	requestBody := make(chan []byte, 1)
	requestReadError := make(chan error, 1)
	responseWriteError := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestPath <- request.URL.Path
		body, readErr := io.ReadAll(request.Body)
		requestBody <- body
		requestReadError <- readErr
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := io.WriteString(w, "data: {\"id\":\"chatcmpl-probe\",\"object\":\"chat.completion.chunk\",\"created\":1721820000,\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"chatcmpl-probe\",\"object\":\"chat.completion.chunk\",\"created\":1721820000,\"model\":\"gpt-4o-mini\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\ndata: [DONE]\n\n")
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
	require.NoError(t, <-requestReadError)
	require.NoError(t, <-responseWriteError)
	var upstreamRequest dto.GeneralOpenAIRequest
	require.NoError(t, common.Unmarshal(<-requestBody, &upstreamRequest))
	require.NotNil(t, upstreamRequest.Stream)
	require.True(t, *upstreamRequest.Stream)
	require.NotNil(t, upstreamRequest.StreamOptions)
	require.True(t, upstreamRequest.StreamOptions.IncludeUsage)
	require.NotNil(t, upstreamRequest.MaxTokens)
	require.Equal(t, uint(8192), *upstreamRequest.MaxTokens)

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

func TestProbeChannelUsesNativeClaudeStreamWithOneHourCache(t *testing.T) {
	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })
	database := setupModelListControllerTestDB(t)
	service.InitHttpClient()
	require.NoError(t, database.Create(&model.User{
		Id:       1,
		Username: "root-claude-probe",
		Password: "password",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Quota:    1000,
		Group:    "default",
	}).Error)

	requestPath := make(chan string, 1)
	requestBody := make(chan []byte, 1)
	requestReadError := make(chan error, 1)
	responseWriteError := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestPath <- request.URL.Path
		body, readErr := io.ReadAll(request.Body)
		requestBody <- body
		requestReadError <- readErr
		w.Header().Set("Content-Type", "text/event-stream")
		_, writeErr := io.WriteString(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-probe\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-opus-4-7\",\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"OK\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":1}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
		responseWriteError <- writeErr
	}))
	defer upstream.Close()

	baseURL := upstream.URL
	channel := &model.Channel{
		Id:      11,
		Type:    constant.ChannelTypeAnthropic,
		Key:     "test-key",
		Status:  common.ChannelStatusEnabled,
		Name:    "claude-probe-channel",
		BaseURL: &baseURL,
		Models:  "claude-opus-4-7-high",
		Group:   "standard",
	}
	require.NoError(t, database.Create(channel).Error)

	result := ProbeChannel(nil, channel, "claude-opus-4-7-high", "standard")
	require.Equal(t, ChannelProbeStatusSucceeded, result.Status)
	require.Equal(t, "/v1/messages", <-requestPath)
	require.NoError(t, <-requestReadError)
	require.NoError(t, <-responseWriteError)

	var upstreamRequest dto.ClaudeRequest
	require.NoError(t, common.Unmarshal(<-requestBody, &upstreamRequest))
	require.NotNil(t, upstreamRequest.Stream)
	require.True(t, *upstreamRequest.Stream)
	require.NotNil(t, upstreamRequest.MaxTokens)
	require.Equal(t, uint(8192), *upstreamRequest.MaxTokens)
	require.Equal(t, "claude-opus-4-7", upstreamRequest.Model)
	require.NotNil(t, upstreamRequest.Thinking)
	require.Equal(t, "adaptive", upstreamRequest.Thinking.Type)
	require.Equal(t, "summarized", upstreamRequest.Thinking.Display)
	require.JSONEq(t, `{"effort":"high"}`, string(upstreamRequest.OutputConfig))
	system, err := common.Any2Type[[]dto.ClaudeMediaMessage](upstreamRequest.System)
	require.NoError(t, err)
	require.Len(t, system, 1)
	var cacheControl map[string]string
	require.NoError(t, common.Unmarshal(system[0].CacheControl, &cacheControl))
	require.Equal(t, map[string]string{"type": "ephemeral", "ttl": "1h"}, cacheControl)
}

func TestBuildTestRequestUsesNormalGeminiStreamingParameters(t *testing.T) {
	request, ok := buildTestRequest(
		"gemini-2.5-pro",
		string(constant.EndpointTypeOpenAI),
		&model.Channel{Type: constant.ChannelTypeOpenAI},
		true,
		channelTestRequestProfileContinuityProbe,
	).(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, request.Stream)
	require.True(t, *request.Stream)
	require.NotNil(t, request.StreamOptions)
	require.True(t, request.StreamOptions.IncludeUsage)
	require.NotNil(t, request.MaxTokens)
	require.Equal(t, uint(8192), *request.MaxTokens)
	require.Equal(t, "low", request.ReasoningEffort)
}

func TestProbeChannelUsesClaudeResponsesCompatibilityPolicy(t *testing.T) {
	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	globalSettings := model_setting.GetGlobalSettings()
	oldPolicy := globalSettings.ChatCompletionsToResponsesPolicy
	globalSettings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
		Enabled:       true,
		ChannelTypes:  []int{constant.ChannelTypeOpenAI},
		ModelPatterns: []string{`^claude-sonnet-4-6$`},
	}
	t.Cleanup(func() { globalSettings.ChatCompletionsToResponsesPolicy = oldPolicy })

	database := setupModelListControllerTestDB(t)
	service.InitHttpClient()
	require.NoError(t, database.Create(&model.User{
		Id:       1,
		Username: "root-claude-responses-probe",
		Password: "password",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Quota:    1000,
		Group:    "default",
	}).Error)

	requestPath := make(chan string, 1)
	requestBody := make(chan []byte, 1)
	requestReadError := make(chan error, 1)
	responseWriteError := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestPath <- request.URL.Path
		body, readErr := io.ReadAll(request.Body)
		requestBody <- body
		requestReadError <- readErr
		w.Header().Set("Content-Type", "text/event-stream")
		_, writeErr := io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_claude_probe\",\"model\":\"claude-sonnet-4-6\",\"created_at\":1721820000}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_claude_probe\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n")
		responseWriteError <- writeErr
	}))
	defer upstream.Close()

	baseURL := upstream.URL
	channel := &model.Channel{
		Id:      13,
		Type:    constant.ChannelTypeOpenAI,
		Key:     "test-key",
		Status:  common.ChannelStatusEnabled,
		Name:    "claude-responses-probe-channel",
		BaseURL: &baseURL,
		Models:  "claude-sonnet-4-6",
		Group:   "standard",
	}
	require.NoError(t, database.Create(channel).Error)

	result := ProbeChannel(nil, channel, "claude-sonnet-4-6", "standard")
	require.Equal(t, ChannelProbeStatusSucceeded, result.Status)
	require.Equal(t, "/v1/responses", <-requestPath)
	require.NoError(t, <-requestReadError)
	require.NoError(t, <-responseWriteError)

	var upstreamRequest dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(<-requestBody, &upstreamRequest))
	require.Equal(t, "claude-sonnet-4-6", upstreamRequest.Model)
	require.NotNil(t, upstreamRequest.Stream)
	require.True(t, *upstreamRequest.Stream)
	require.NotEmpty(t, upstreamRequest.Input)
}

func TestBuildTestRequestKeepsManualChannelTestProfile(t *testing.T) {
	request, ok := buildTestRequest(
		"gpt-4o-mini",
		string(constant.EndpointTypeOpenAI),
		&model.Channel{Type: constant.ChannelTypeOpenAI},
		true,
		channelTestRequestProfileDefault,
	).(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, request.MaxTokens)
	require.Equal(t, uint(16), *request.MaxTokens)

	claudeRequest, ok := buildTestRequest(
		"claude-sonnet-4-6",
		string(constant.EndpointTypeAnthropic),
		&model.Channel{Type: constant.ChannelTypeAnthropic},
		true,
		channelTestRequestProfileDefault,
	).(*dto.GeneralOpenAIRequest)
	require.True(t, ok)
	require.NotNil(t, claudeRequest.MaxTokens)
	require.Equal(t, uint(16), *claudeRequest.MaxTokens)
}

func TestProbeChannelUsesConfiguredChatToResponsesCompatibilityForCodex(t *testing.T) {
	oldStreamingTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldStreamingTimeout })

	globalSettings := model_setting.GetGlobalSettings()
	oldPolicy := globalSettings.ChatCompletionsToResponsesPolicy
	globalSettings.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
		Enabled:       true,
		ChannelTypes:  []int{constant.ChannelTypeCodex},
		ModelPatterns: []string{`^gpt-5$`},
	}
	t.Cleanup(func() { globalSettings.ChatCompletionsToResponsesPolicy = oldPolicy })

	database := setupModelListControllerTestDB(t)
	service.InitHttpClient()
	require.NoError(t, database.Create(&model.User{
		Id:       1,
		Username: "root-codex-probe",
		Password: "password",
		Role:     common.RoleRootUser,
		Status:   common.UserStatusEnabled,
		Quota:    1000,
		Group:    "default",
	}).Error)

	requestPath := make(chan string, 1)
	requestBody := make(chan []byte, 1)
	requestReadError := make(chan error, 1)
	responseWriteError := make(chan error, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestPath <- request.URL.Path
		body, readErr := io.ReadAll(request.Body)
		requestBody <- body
		requestReadError <- readErr
		w.Header().Set("Content-Type", "text/event-stream")
		_, writeErr := io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_probe\",\"model\":\"gpt-5\",\"created_at\":1721820000}}\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_probe\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\ndata: [DONE]\n\n")
		responseWriteError <- writeErr
	}))
	defer upstream.Close()

	baseURL := upstream.URL
	channelSetting := `{"system_prompt":"probe-system-prompt"}`
	channel := &model.Channel{
		Id:      12,
		Type:    constant.ChannelTypeCodex,
		Key:     `{"access_token":"test-token","account_id":"test-account"}`,
		Status:  common.ChannelStatusEnabled,
		Name:    "codex-probe-channel",
		BaseURL: &baseURL,
		Models:  "gpt-5",
		Group:   "standard",
		Setting: &channelSetting,
	}
	require.NoError(t, database.Create(channel).Error)

	result := ProbeChannel(nil, channel, "gpt-5", "standard")
	require.Equal(t, ChannelProbeStatusSucceeded, result.Status)
	require.Equal(t, "/backend-api/codex/responses", <-requestPath)
	require.NoError(t, <-requestReadError)
	require.NoError(t, <-responseWriteError)

	var upstreamRequest dto.OpenAIResponsesRequest
	require.NoError(t, common.Unmarshal(<-requestBody, &upstreamRequest))
	require.NotNil(t, upstreamRequest.Stream)
	require.True(t, *upstreamRequest.Stream)
	require.JSONEq(t, "false", string(upstreamRequest.Store))
	require.NotEmpty(t, upstreamRequest.Input)
	var instructions string
	require.NoError(t, common.Unmarshal(upstreamRequest.Instructions, &instructions))
	require.Equal(t, "probe-system-prompt", instructions)
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
