package channel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessHeaderOverrideNeverPassesContinuityCacheEvidence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		rule string
	}{
		{name: "wildcard", rule: "*"},
		{name: "regex", rule: "regex:^x-continuity-"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request.Header.Set(service.ContinuityCacheEvidenceHeader, "must-not-leave-new-api")
			c.Request.Header.Set("X-Allowed-Trace", "trace-1")

			info := &relaycommon.RelayInfo{
				ChannelMeta: &relaycommon.ChannelMeta{
					HeadersOverride: map[string]interface{}{tt.rule: ""},
				},
			}
			headers, err := processHeaderOverride(info, c)
			require.NoError(t, err)

			assert.NotContains(t, headers, service.ContinuityCacheEvidenceHeaderLower)
			if tt.rule == "*" {
				assert.Equal(t, "trace-1", headers["x-allowed-trace"])
			}
		})
	}
}
