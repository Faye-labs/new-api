package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRelayOutcomeSucceededUsesFinalClientVisibleResult(t *testing.T) {
	gin.SetMode(gin.TestMode)
	newContext := func() (*gin.Context, *httptest.ResponseRecorder) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return ctx, recorder
	}

	t.Run("ordinary success", func(t *testing.T) {
		ctx, _ := newContext()
		assert.True(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{}, nil))
	})

	t.Run("returned relay error", func(t *testing.T) {
		ctx, _ := newContext()
		assert.False(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{}, &types.NewAPIError{}))
	})

	t.Run("client gone stream", func(t *testing.T) {
		ctx, _ := newContext()
		status := relaycommon.NewStreamStatus()
		status.SetEndReason(relaycommon.StreamEndReasonClientGone, context.Canceled)
		assert.False(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{StreamStatus: status}, nil))
	})

	t.Run("normal stream with soft error", func(t *testing.T) {
		ctx, _ := newContext()
		status := relaycommon.NewStreamStatus()
		status.SetEndReason(relaycommon.StreamEndReasonHandlerStop, nil)
		status.RecordError("provider emitted an error event")
		assert.False(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{StreamStatus: status}, nil))
	})

	t.Run("normal eof stream", func(t *testing.T) {
		ctx, _ := newContext()
		status := relaycommon.NewStreamStatus()
		status.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
		assert.True(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{StreamStatus: status}, nil))
	})

	t.Run("handler wrote error response", func(t *testing.T) {
		ctx, _ := newContext()
		ctx.Status(http.StatusBadRequest)
		assert.False(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{}, nil))
	})

	t.Run("request context canceled", func(t *testing.T) {
		ctx, _ := newContext()
		requestContext, cancel := context.WithCancel(ctx.Request.Context())
		cancel()
		ctx.Request = ctx.Request.WithContext(requestContext)
		assert.False(t, relayOutcomeSucceeded(ctx, &relaycommon.RelayInfo{}, nil))
	})
}
