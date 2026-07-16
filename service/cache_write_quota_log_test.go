package service

import (
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAppendCacheWriteQuota1hStandardUsesFrozenBillingRatios(t *testing.T) {
	priceData := types.PriceData{
		ModelRatio:           2,
		CacheCreation1hRatio: 4,
		GroupRatioInfo: types.GroupRatioInfo{
			GroupRatio: 3,
		},
	}
	priceData.AddOtherRatio("request_multiplier", 0.5)
	relayInfo := &relaycommon.RelayInfo{PriceData: priceData}
	summary := textQuotaSummary{
		CacheCreationTokens1h: 10,
		CacheCreationRatio1h:  4,
		ModelRatio:            2,
		GroupRatio:            3,
	}
	other := map[string]interface{}{}

	appendCacheWriteQuota1h(other, relayInfo, nil, summary, false, nil)

	assert.Equal(t, 120, other[cacheWriteQuota1hField])
	assert.NotContains(t, other, cacheWriteQuota1hStatusField)
}

func TestAppendCacheWriteQuota1hStandardMarksFlatPriceUnsupported(t *testing.T) {
	relayInfo := &relaycommon.RelayInfo{PriceData: types.PriceData{UsePrice: true}}
	other := map[string]interface{}{}

	appendCacheWriteQuota1h(other, relayInfo, nil, textQuotaSummary{CacheCreationTokens1h: 10}, false, nil)

	assert.NotContains(t, other, cacheWriteQuota1hField)
	assert.Equal(t, "unsupported_flat_price", other[cacheWriteQuota1hStatusField])
}

func TestAppendCacheWriteQuota1hTieredKeepsLenFixed(t *testing.T) {
	exprString := `len <= 50000 ? tier("short", p + cc1h * 2) : tier("long", p * 100 + cc1h * 6)`
	snapshot := &billingexpr.BillingSnapshot{
		BillingMode:  "tiered_expr",
		ExprString:   exprString,
		ExprHash:     billingexpr.ExprHashString(exprString),
		GroupRatio:   1,
		QuotaPerUnit: 1_000_000,
	}
	usage := &dto.Usage{
		PromptTokens:                      1_000,
		UsageSemantic:                     "anthropic",
		ClaudeCacheCreation1hTokens:       100_000,
		ClaudeCacheCreation5mTokens:       0,
	}
	usedVars := billingexpr.UsedVars(exprString)
	params := BuildTieredTokenParams(usage, true, usedVars)
	require.Equal(t, float64(101_000), params.Len)
	actual, err := billingexpr.ComputeTieredQuota(snapshot, params)
	require.NoError(t, err)
	require.Equal(t, "long", actual.MatchedTier)

	relayInfo := &relaycommon.RelayInfo{TieredBillingSnapshot: snapshot}
	other := map[string]interface{}{}
	appendCacheWriteQuota1h(other, relayInfo, usage, textQuotaSummary{
		CacheCreationTokens1h: 100_000,
		IsClaudeUsageSemantic: true,
	}, true, &actual)

	// Fixed Len keeps the counterfactual in the long tier:
	// (1000*100 + 100000*6) - (1000*100) = 600000 quota.
	assert.Equal(t, 600_000, other[cacheWriteQuota1hField])
	assert.NotContains(t, other, cacheWriteQuota1hStatusField)
}

func TestAppendCacheWriteQuota1hTieredRequiresExplicitCC1h(t *testing.T) {
	exprString := `tier("base", p * 2)`
	snapshot := &billingexpr.BillingSnapshot{
		BillingMode:  "tiered_expr",
		ExprString:   exprString,
		ExprHash:     billingexpr.ExprHashString(exprString),
		GroupRatio:   1,
		QuotaPerUnit: 1_000_000,
	}
	usage := &dto.Usage{
		PromptTokens:                1_000,
		UsageSemantic:               "anthropic",
		ClaudeCacheCreation1hTokens: 100_000,
	}
	actual, err := billingexpr.ComputeTieredQuota(snapshot, BuildTieredTokenParams(usage, true, billingexpr.UsedVars(exprString)))
	require.NoError(t, err)

	other := map[string]interface{}{}
	appendCacheWriteQuota1h(other, &relaycommon.RelayInfo{TieredBillingSnapshot: snapshot}, usage, textQuotaSummary{
		CacheCreationTokens1h: 100_000,
		IsClaudeUsageSemantic: true,
	}, true, &actual)

	assert.NotContains(t, other, cacheWriteQuota1hField)
	assert.Equal(t, "unsupported_tiered_cc1h_not_explicit", other[cacheWriteQuota1hStatusField])
}

func TestAppendCacheWriteQuota1hTieredRejectsVolatileExpression(t *testing.T) {
	exprString := `minute("UTC") >= 0 ? tier("base", p + cc1h * 6) : tier("never", p)`
	snapshot := &billingexpr.BillingSnapshot{
		BillingMode:  "tiered_expr",
		ExprString:   exprString,
		ExprHash:     billingexpr.ExprHashString(exprString),
		GroupRatio:   1,
		QuotaPerUnit: 1_000_000,
	}
	usage := &dto.Usage{
		PromptTokens:                1_000,
		UsageSemantic:               "anthropic",
		ClaudeCacheCreation1hTokens: 100_000,
	}
	usedVars := billingexpr.UsedVars(exprString)
	require.True(t, usedVars["minute"])
	actual, err := billingexpr.ComputeTieredQuota(snapshot, BuildTieredTokenParams(usage, true, usedVars))
	require.NoError(t, err)

	other := map[string]interface{}{}
	appendCacheWriteQuota1h(other, &relaycommon.RelayInfo{TieredBillingSnapshot: snapshot}, usage, textQuotaSummary{
		CacheCreationTokens1h: 100_000,
		IsClaudeUsageSemantic: true,
	}, true, &actual)

	assert.NotContains(t, other, cacheWriteQuota1hField)
	assert.Equal(t, "unsupported_tiered_volatile_expr", other[cacheWriteQuota1hStatusField])
}
