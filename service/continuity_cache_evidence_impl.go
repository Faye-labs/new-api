package service

import (
	"encoding/base64"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

const (
	ContinuityCacheEvidenceHeader      = "X-Continuity-Cache-Evidence"
	ContinuityCacheEvidenceHeaderLower = "x-continuity-cache-evidence"

	continuityCacheEvidenceContextKey = "continuity_cache_evidence"
	continuityCacheEvidenceMaxBytes   = 6144
)

const continuityCacheReliefKeyFingerprintDomain = "continuity-cache-relief:key:v1\x00"

// CaptureContinuityCacheEvidence removes the Continuity-only evidence header
// before any relay/header-override code can inspect it. A structurally valid
// signed envelope is retained on the Gin context for consume-log auditing; the
// settlement owner verifies the HMAC before trusting its payload.
func CaptureContinuityCacheEvidence(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}

	values := c.Request.Header.Values(ContinuityCacheEvidenceHeader)
	// Internal evidence must never be forwarded, including malformed or
	// duplicated values.
	c.Request.Header.Del(ContinuityCacheEvidenceHeader)
	if len(values) != 1 {
		return
	}

	envelope := strings.TrimSpace(values[0])
	if !isStructurallyValidContinuityCacheEvidence(envelope) {
		return
	}
	c.Set(continuityCacheEvidenceContextKey, envelope)
}

func isStructurallyValidContinuityCacheEvidence(envelope string) bool {
	if envelope == "" || len(envelope) > continuityCacheEvidenceMaxBytes {
		return false
	}
	parts := strings.Split(envelope, ".")
	if len(parts) != 3 || parts[0] != "v1" || parts[1] == "" || parts[2] == "" {
		return false
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payloadBytes) == 0 {
		return false
	}
	var payload struct {
		Version int `json:"v"`
	}
	if err := common.Unmarshal(payloadBytes, &payload); err != nil || payload.Version != 1 {
		return false
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	return err == nil && len(signature) == 32
}

func appendContinuityCacheEvidenceAdminInfo(c *gin.Context, relayInfo *relaycommon.RelayInfo, adminInfo map[string]interface{}) {
	if c == nil || relayInfo == nil || adminInfo == nil {
		return
	}
	envelope := c.GetString(continuityCacheEvidenceContextKey)
	if envelope == "" {
		return
	}

	firstResponseAtMs := time.Now().UnixMilli()
	if relayInfo.HasSendResponse() {
		firstResponseAtMs = relayInfo.FirstResponseTime.UnixMilli()
	}
	evidenceInfo := map[string]interface{}{
		"envelope":                envelope,
		"request_started_at_ms":   relayInfo.StartTime.UnixMilli(),
		"first_response_at_ms":    firstResponseAtMs,
		"resolved_upstream_model": relayInfo.UpstreamModelName,
	}
	if relayInfo.ChannelMeta != nil && relayInfo.ChannelMeta.ApiKey != "" {
		evidenceInfo["upstream_key_fingerprint"] = common.GenerateHMAC(
			continuityCacheReliefKeyFingerprintDomain + relayInfo.ChannelMeta.ApiKey,
		)
	}
	adminInfo["continuity_cache_evidence"] = evidenceInfo
}
