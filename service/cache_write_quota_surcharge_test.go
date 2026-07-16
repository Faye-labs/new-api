package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendCacheWriteQuota1hTieredKeepsToolSurchargeFixed(t *testing.T) {
	exprString := `tier("base", p * 0.2 + cc1h * 0.4)`
	snapshot := &billingexpr.BillingSnapshot{
		BillingMode:  "tiered_expr",
		ExprString:   exprString,
		ExprHash:     billingexpr.ExprHashString(exprString),
		GroupRatio:   1,
		QuotaPerUnit: 1_000_000,
	}
	usage := &dto.Usage{
		PromptTokens:                1,
		UsageSemantic:               "anthropic",
		ClaudeCacheCreation1hTokens: 1,
	}
	actual, err := billingexpr.ComputeTieredQuota(snapshot, BuildTieredTokenParams(usage, true, billingexpr.UsedVars(exprString)))
	require.NoError(t, err)
	require.Equal(t, 1, actual.ActualQuotaAfterGroup)

	other := map[string]interface{}{}
	appendCacheWriteQuota1h(other, &relaycommon.RelayInfo{TieredBillingSnapshot: snapshot}, usage, textQuotaSummary{
		CacheCreationTokens1h: 1,
		IsClaudeUsageSemantic: true,
		ToolCallSurchargeQuota: decimal.RequireFromString("0.4"),
	}, true, &actual)

	// The actual settlement rounds (tier quota + surcharge) together when a
	// surcharge exists: round(0.6+0.4)-round(0.2+0.4) = 1-1 = 0.
	assert.Equal(t, 0, other[cacheWriteQuota1hField])
	assert.NotContains(t, other, cacheWriteQuota1hStatusField)
}
