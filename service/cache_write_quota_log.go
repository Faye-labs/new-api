package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/shopspring/decimal"
)

const (
	cacheWriteQuota1hField       = "cache_write_quota_1h"
	cacheWriteQuota1hStatusField = "cache_write_quota_1h_status"
)

var volatileTieredBillingIdentifiers = []string{
	"hour",
	"minute",
	"weekday",
	"month",
	"day",
}

func appendCacheWriteQuota1h(
	other map[string]interface{},
	relayInfo *relaycommon.RelayInfo,
	usage *dto.Usage,
	summary textQuotaSummary,
	tieredBillingApplied bool,
	tieredResult *billingexpr.TieredResult,
) {
	if other == nil || relayInfo == nil || summary.CacheCreationTokens1h <= 0 {
		return
	}

	if tieredBillingApplied {
		quota, status, ok := calculateTieredCacheWriteQuota1h(relayInfo, usage, summary, tieredResult)
		if ok {
			other[cacheWriteQuota1hField] = quota
		} else {
			other[cacheWriteQuota1hStatusField] = status
		}
		return
	}

	quota, status, ok := calculateStandardCacheWriteQuota1h(relayInfo, summary)
	if ok {
		other[cacheWriteQuota1hField] = quota
	} else {
		other[cacheWriteQuota1hStatusField] = status
	}
}

func calculateStandardCacheWriteQuota1h(relayInfo *relaycommon.RelayInfo, summary textQuotaSummary) (int, string, bool) {
	if relayInfo.PriceData.UsePrice {
		return 0, "unsupported_flat_price", false
	}

	quotaDecimal := decimal.NewFromInt(int64(summary.CacheCreationTokens1h)).
		Mul(decimal.NewFromFloat(summary.CacheCreationRatio1h)).
		Mul(decimal.NewFromFloat(summary.ModelRatio)).
		Mul(decimal.NewFromFloat(summary.GroupRatio))
	quotaDecimal = relayInfo.PriceData.ApplyOtherRatiosToDecimal(quotaDecimal)
	quota, clamp := common.QuotaFromDecimalChecked(quotaDecimal)
	if clamp != nil {
		return 0, "unsupported_quota_clamped", false
	}
	return quota, "", true
}

func calculateTieredCacheWriteQuota1h(
	relayInfo *relaycommon.RelayInfo,
	usage *dto.Usage,
	summary textQuotaSummary,
	actualResult *billingexpr.TieredResult,
) (int, string, bool) {
	if usage == nil || actualResult == nil {
		return 0, "unsupported_tiered_no_result", false
	}
	snapshot := relayInfo.TieredBillingSnapshot
	if snapshot == nil || snapshot.BillingMode != "tiered_expr" {
		return 0, "unsupported_tiered_snapshot", false
	}
	if actualResult.Clamp != nil {
		return 0, "unsupported_quota_clamped", false
	}

	usedVars := billingexpr.UsedVars(snapshot.ExprString)
	if !usedVars["cc1h"] {
		return 0, "unsupported_tiered_cc1h_not_explicit", false
	}
	for _, identifier := range volatileTieredBillingIdentifiers {
		if usedVars[identifier] {
			return 0, "unsupported_tiered_volatile_expr", false
		}
	}

	params := BuildTieredTokenParams(usage, summary.IsClaudeUsageSemantic, usedVars)
	if params.CC1h <= 0 {
		return 0, "unsupported_tiered_missing_cc1h", false
	}
	// Keep Len and every non-CC1h input fixed. Recomputing Len after removing
	// CC1h could cross a long-context tier and overstate the write component.
	withoutCC1h := params
	withoutCC1h.CC1h = 0

	requestInput := billingexpr.RequestInput{}
	if relayInfo.BillingRequestInput != nil {
		requestInput = *relayInfo.BillingRequestInput
	}
	counterfactual, err := billingexpr.ComputeTieredQuotaWithRequest(snapshot, withoutCC1h, requestInput)
	if err != nil {
		return 0, "unsupported_tiered_eval", false
	}
	if counterfactual.Clamp != nil {
		return 0, "unsupported_quota_clamped", false
	}

	actualQuota, actualClamp := composeTieredAuditQuota(
		actualResult,
		snapshot.GroupRatio,
		summary.ToolCallSurchargeQuota,
	)
	counterfactualQuota, counterfactualClamp := composeTieredAuditQuota(
		&counterfactual,
		snapshot.GroupRatio,
		summary.ToolCallSurchargeQuota,
	)
	if actualClamp != nil || counterfactualClamp != nil {
		return 0, "unsupported_quota_clamped", false
	}

	marginalQuota := actualQuota - counterfactualQuota
	if marginalQuota < 0 {
		return 0, "unsupported_tiered_negative_marginal", false
	}
	return marginalQuota, "", true
}

func composeTieredAuditQuota(
	result *billingexpr.TieredResult,
	groupRatio float64,
	surcharge decimal.Decimal,
) (int, *common.QuotaClamp) {
	if surcharge.IsZero() {
		return result.ActualQuotaAfterGroup, result.Clamp
	}
	return common.QuotaFromDecimalChecked(
		decimal.NewFromFloat(result.ActualQuotaBeforeGroup).
			Mul(decimal.NewFromFloat(groupRatio)).
			Add(surcharge),
	)
}
