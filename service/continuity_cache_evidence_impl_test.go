package service

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validContinuityCacheEvidenceEnvelope() string {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"cache_partition_hash":"partition-1"}`))
	signature := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	return "v1." + payload + "." + signature
}

func newContinuityCacheEvidenceTestContext() *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func TestCaptureContinuityCacheEvidenceStripsAndLogsValidEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newContinuityCacheEvidenceTestContext()
	envelope := validContinuityCacheEvidenceEnvelope()
	c.Request.Header.Set(ContinuityCacheEvidenceHeader, envelope)

	CaptureContinuityCacheEvidence(c)

	require.Empty(t, c.Request.Header.Get(ContinuityCacheEvidenceHeader))
	require.Equal(t, envelope, c.GetString(continuityCacheEvidenceContextKey))

	start := time.UnixMilli(1_720_000_000_000)
	firstResponse := start.Add(250 * time.Millisecond)
	adminInfo := map[string]interface{}{"multi_key_index": 3}
	appendContinuityCacheEvidenceAdminInfo(c, &relaycommon.RelayInfo{
		StartTime:         start,
		FirstResponseTime: firstResponse,
		UpstreamModelName: "claude-sonnet-resolved",
	}, adminInfo)

	require.Equal(t, 3, adminInfo["multi_key_index"])
	logged, ok := adminInfo["continuity_cache_evidence"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, envelope, logged["envelope"])
	assert.Equal(t, start.UnixMilli(), logged["request_started_at_ms"])
	assert.Equal(t, firstResponse.UnixMilli(), logged["first_response_at_ms"])
	assert.Equal(t, "claude-sonnet-resolved", logged["resolved_upstream_model"])
}

func TestCaptureContinuityCacheEvidenceRejectsButAlwaysStripsInvalidValues(t *testing.T) {
	gin.SetMode(gin.TestMode)
	validEnvelope := validContinuityCacheEvidenceEnvelope()
	validPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"v":1}`))
	validSignature := base64.RawURLEncoding.EncodeToString(make([]byte, 32))

	tests := []struct {
		name   string
		values []string
	}{
		{name: "missing", values: nil},
		{name: "duplicate", values: []string{validEnvelope, validEnvelope}},
		{name: "too long", values: []string{strings.Repeat("a", continuityCacheEvidenceMaxBytes+1)}},
		{name: "wrong version", values: []string{"v2." + validPayload + "." + validSignature}},
		{name: "wrong payload version", values: []string{"v1." + base64.RawURLEncoding.EncodeToString([]byte(`{"v":2}`)) + "." + validSignature}},
		{name: "invalid payload base64url", values: []string{"v1.*." + validSignature}},
		{name: "payload is not an object", values: []string{"v1." + base64.RawURLEncoding.EncodeToString([]byte(`[]`)) + "." + validSignature}},
		{name: "wrong signature length", values: []string{"v1." + validPayload + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 31))}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newContinuityCacheEvidenceTestContext()
			for _, value := range tt.values {
				c.Request.Header.Add(ContinuityCacheEvidenceHeader, value)
			}

			CaptureContinuityCacheEvidence(c)

			assert.Empty(t, c.Request.Header.Values(ContinuityCacheEvidenceHeader))
			assert.Empty(t, c.GetString(continuityCacheEvidenceContextKey))
		})
	}
}

func TestAppendContinuityCacheEvidenceAdminInfoUsesLogTimeWhenFirstResponseIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newContinuityCacheEvidenceTestContext()
	c.Request.Header.Set(ContinuityCacheEvidenceHeader, validContinuityCacheEvidenceEnvelope())
	CaptureContinuityCacheEvidence(c)

	start := time.UnixMilli(1_720_000_000_000)
	adminInfo := map[string]interface{}{}
	before := time.Now().UnixMilli()
	appendContinuityCacheEvidenceAdminInfo(c, &relaycommon.RelayInfo{
		StartTime:         start,
		FirstResponseTime: start.Add(-time.Second),
		UpstreamModelName: "",
	}, adminInfo)
	after := time.Now().UnixMilli()

	logged, ok := adminInfo["continuity_cache_evidence"].(map[string]interface{})
	require.True(t, ok)
	firstResponseAtMs, ok := logged["first_response_at_ms"].(int64)
	require.True(t, ok)
	assert.Positive(t, firstResponseAtMs)
	assert.GreaterOrEqual(t, firstResponseAtMs, before)
	assert.LessOrEqual(t, firstResponseAtMs, after)
	assert.Equal(t, "", logged["resolved_upstream_model"])
	assert.NotContains(t, logged, "upstream_key_fingerprint")
}

func TestAppendContinuityCacheEvidenceAdminInfoLogsOpaqueStableUpstreamKeyFingerprint(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := newContinuityCacheEvidenceTestContext()
	c.Request.Header.Set(ContinuityCacheEvidenceHeader, validContinuityCacheEvidenceEnvelope())
	CaptureContinuityCacheEvidence(c)

	start := time.UnixMilli(1_720_000_000_000)
	appendForKey := func(apiKey string) map[string]interface{} {
		adminInfo := map[string]interface{}{}
		appendContinuityCacheEvidenceAdminInfo(c, &relaycommon.RelayInfo{
			StartTime:         start,
			FirstResponseTime: start.Add(time.Millisecond),
			ChannelMeta:       &relaycommon.ChannelMeta{ApiKey: apiKey},
		}, adminInfo)
		logged, ok := adminInfo["continuity_cache_evidence"].(map[string]interface{})
		require.True(t, ok)
		return logged
	}

	keyA := "sk-upstream-secret-a"
	keyB := "sk-upstream-secret-b"
	first := appendForKey(keyA)
	second := appendForKey(keyA)
	different := appendForKey(keyB)
	empty := appendForKey("")

	fingerprint, ok := first["upstream_key_fingerprint"].(string)
	require.True(t, ok)
	differentFingerprint, ok := different["upstream_key_fingerprint"].(string)
	require.True(t, ok)
	assert.Equal(t, common.GenerateHMAC(continuityCacheReliefKeyFingerprintDomain+keyA), fingerprint)
	assert.Equal(t, fingerprint, second["upstream_key_fingerprint"])
	assert.NotEqual(t, fingerprint, differentFingerprint)
	assert.NotContains(t, empty, "upstream_key_fingerprint")
	assert.NotContains(t, fingerprint, keyA)
	assert.NotContains(t, differentFingerprint, keyB)

	encodedAdminInfo, err := common.Marshal([]map[string]interface{}{
		{"continuity_cache_evidence": first},
		{"continuity_cache_evidence": different},
	})
	require.NoError(t, err)
	assert.NotContains(t, string(encodedAdminInfo), keyA)
	assert.NotContains(t, string(encodedAdminInfo), keyB)
}
