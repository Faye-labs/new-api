package relay

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/samber/lo"

	"github.com/gin-gonic/gin"
)

func TextHelper(c *gin.Context, info *relaycommon.RelayInfo) (newAPIError *types.NewAPIError) {
	info.InitChannelMeta(c)

	textReq, ok := info.Request.(*dto.GeneralOpenAIRequest)
	if !ok {
		return types.NewErrorWithStatusCode(fmt.Errorf("invalid request type, expected dto.GeneralOpenAIRequest, got %T", info.Request), types.ErrorCodeInvalidRequest, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
	}

	request, err := common.DeepCopy(textReq)
	if err != nil {
		return types.NewError(fmt.Errorf("failed to copy request to GeneralOpenAIRequest: %w", err), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	if request.WebSearchOptions != nil {
		c.Set("chat_completion_web_search_context_size", request.WebSearchOptions.SearchContextSize)
	}

	err = helper.ModelMappedHelper(c, info, request)
	if err != nil {
		return types.NewError(err, types.ErrorCodeChannelModelMappedError, types.ErrOptionWithSkipRetry())
	}

	NormalizeChatCompletionsStreamOptions(info, request)

	adaptor := GetAdaptor(info.ApiType)
	if adaptor == nil {
		return types.NewError(fmt.Errorf("invalid api type: %d", info.ApiType), types.ErrorCodeInvalidApiType, types.ErrOptionWithSkipRetry())
	}
	adaptor.Init(info)

	routing := ResolveChatCompletionsRouting(info)
	if routing.UseResponsesCompatibility {
		ApplyChatCompletionsSystemPrompt(c, info, request)
		usage, newApiErr := chatCompletionsViaResponses(c, info, adaptor, request)
		if newApiErr != nil {
			return newApiErr
		}

		var containAudioTokens = usage.CompletionTokenDetails.AudioTokens > 0 || usage.PromptTokensDetails.AudioTokens > 0
		var containsAudioRatios = ratio_setting.ContainsAudioRatio(info.OriginModelName) || ratio_setting.ContainsAudioCompletionRatio(info.OriginModelName)

		if containAudioTokens && containsAudioRatios {
			service.PostAudioConsumeQuota(c, info, usage, "")
		} else {
			service.PostTextConsumeQuota(c, info, usage, nil)
		}
		return nil
	}

	var requestBody io.Reader

	if routing.PassThroughGlobal || info.ChannelSetting.PassThroughBodyEnabled {
		storage, err := common.GetBodyStorage(c)
		if err != nil {
			return types.NewErrorWithStatusCode(err, types.ErrorCodeReadRequestBodyFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry())
		}
		if common.DebugEnabled {
			if debugBytes, bErr := storage.Bytes(); bErr == nil {
				logger.LogDebug(c, "requestBody: %s", debugBytes)
			}
		}
		requestBody = common.ReaderOnly(storage)
	} else {
		convertedRequest, err := adaptor.ConvertOpenAIRequest(c, info, request)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		relaycommon.AppendRequestConversionFromRequest(info, convertedRequest)

		if convertedChatRequest, ok := convertedRequest.(*dto.GeneralOpenAIRequest); ok {
			ApplyChatCompletionsSystemPrompt(c, info, convertedChatRequest)
		}

		jsonData, err := common.Marshal(convertedRequest)
		if err != nil {
			return types.NewError(err, types.ErrorCodeJsonMarshalFailed, types.ErrOptionWithSkipRetry())
		}

		// remove disabled fields for OpenAI API
		jsonData, err = relaycommon.RemoveDisabledFields(jsonData, info.ChannelOtherSettings, info.ChannelSetting.PassThroughBodyEnabled)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}

		// apply param override
		if len(info.ParamOverride) > 0 {
			jsonData, err = relaycommon.ApplyParamOverrideWithRelayInfo(jsonData, info)
			if err != nil {
				return newAPIErrorFromParamOverride(err)
			}
		}

		logger.LogDebug(c, "text request body: %s", jsonData)

		body, size, closer, err := relaycommon.NewOutboundJSONBody(jsonData)
		if err != nil {
			return types.NewError(err, types.ErrorCodeConvertRequestFailed, types.ErrOptionWithSkipRetry())
		}
		defer closer.Close()
		jsonData = nil
		info.UpstreamRequestBodySize = size
		requestBody = body
	}

	var httpResp *http.Response
	resp, err := adaptor.DoRequest(c, info, requestBody)
	if err != nil {
		return types.NewOpenAIError(err, types.ErrorCodeDoRequestFailed, http.StatusInternalServerError)
	}

	statusCodeMappingStr := c.GetString("status_code_mapping")

	if resp != nil {
		httpResp = resp.(*http.Response)
		info.IsStream = info.IsStream || strings.HasPrefix(httpResp.Header.Get("Content-Type"), "text/event-stream")
		if httpResp.StatusCode != http.StatusOK {
			newApiErr := service.RelayErrorHandler(c.Request.Context(), httpResp, false)
			// reset status code 重置状态码
			service.ResetStatusCode(newApiErr, statusCodeMappingStr)
			return newApiErr
		}
	}

	usage, newApiErr := adaptor.DoResponse(c, httpResp, info)
	if newApiErr != nil {
		// reset status code 重置状态码
		service.ResetStatusCode(newApiErr, statusCodeMappingStr)
		return newApiErr
	}

	var containAudioTokens = usage.(*dto.Usage).CompletionTokenDetails.AudioTokens > 0 || usage.(*dto.Usage).PromptTokensDetails.AudioTokens > 0
	var containsAudioRatios = ratio_setting.ContainsAudioRatio(info.OriginModelName) || ratio_setting.ContainsAudioCompletionRatio(info.OriginModelName)

	if containAudioTokens && containsAudioRatios {
		service.PostAudioConsumeQuota(c, info, usage.(*dto.Usage), "")
	} else {
		service.PostTextConsumeQuota(c, info, usage.(*dto.Usage), nil)
	}
	return nil
}

// NormalizeChatCompletionsStreamOptions applies the stream-options contract
// shared by real chat traffic and synthetic status probes.
func NormalizeChatCompletionsStreamOptions(info *relaycommon.RelayInfo, request *dto.GeneralOpenAIRequest) {
	if info == nil || request == nil {
		return
	}
	includeUsage := true
	if request.StreamOptions != nil {
		includeUsage = request.StreamOptions.IncludeUsage
	}

	if !info.SupportStreamOptions || !lo.FromPtrOr(request.Stream, false) {
		request.StreamOptions = nil
	} else if constant.ForceStreamOption {
		request.StreamOptions = &dto.StreamOptions{
			IncludeUsage: true,
		}
	}

	info.ShouldIncludeUsage = includeUsage
}
