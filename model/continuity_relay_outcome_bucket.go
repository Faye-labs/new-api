package model

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ContinuityRelayOutcomeBucketSeconds = int64(time.Minute / time.Second)

// ContinuityRelayOutcomeBucket stores privacy-minimal aggregate outcomes for
// one exact routing group/model pair. It intentionally contains no user,
// token, request, channel credential, prompt, response, or error text.
type ContinuityRelayOutcomeBucket struct {
	Id                     int64  `json:"id" gorm:"primaryKey"`
	GroupKey               string `json:"group_key" gorm:"size:64;uniqueIndex:idx_continuity_relay_outcome_pair_bucket,priority:1"`
	ModelID                string `json:"model_id" gorm:"size:255;uniqueIndex:idx_continuity_relay_outcome_pair_bucket,priority:2"`
	BucketTs               int64  `json:"bucket_ts" gorm:"bigint;uniqueIndex:idx_continuity_relay_outcome_pair_bucket,priority:3;index:idx_continuity_relay_outcome_bucket_ts"`
	RequestCount           int64  `json:"request_count" gorm:"bigint"`
	SuccessCount           int64  `json:"success_count" gorm:"bigint"`
	FailureCount           int64  `json:"failure_count" gorm:"bigint"`
	IgnoredFailureCount    int64  `json:"ignored_failure_count" gorm:"bigint"`
	SuccessLatencySumMs    int64  `json:"success_latency_sum_ms" gorm:"bigint"`
	LatestSuccessAt        int64  `json:"latest_success_at" gorm:"bigint"`
	LatestFailureAt        int64  `json:"latest_failure_at" gorm:"bigint"`
	LatestSuccessLatencyMs int64  `json:"latest_success_latency_ms" gorm:"bigint"`
}

func (ContinuityRelayOutcomeBucket) TableName() string {
	return "continuity_relay_outcome_buckets"
}

func UpsertContinuityRelayOutcomeBucket(bucket *ContinuityRelayOutcomeBucket) error {
	if bucket == nil || bucket.RequestCount <= 0 || bucket.GroupKey == "" || bucket.ModelID == "" {
		return nil
	}
	table := bucket.TableName()
	return DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "group_key"},
			{Name: "model_id"},
			{Name: "bucket_ts"},
		},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"request_count": gorm.Expr(
				table+".request_count + ?",
				bucket.RequestCount,
			),
			"success_count": gorm.Expr(
				table+".success_count + ?",
				bucket.SuccessCount,
			),
			"failure_count": gorm.Expr(
				table+".failure_count + ?",
				bucket.FailureCount,
			),
			"ignored_failure_count": gorm.Expr(
				table+".ignored_failure_count + ?",
				bucket.IgnoredFailureCount,
			),
			"success_latency_sum_ms": gorm.Expr(
				table+".success_latency_sum_ms + ?",
				bucket.SuccessLatencySumMs,
			),
			"latest_success_latency_ms": gorm.Expr(
				"CASE WHEN "+table+".latest_success_at <= ? THEN ? ELSE "+table+".latest_success_latency_ms END",
				bucket.LatestSuccessAt,
				bucket.LatestSuccessLatencyMs,
			),
			"latest_success_at": gorm.Expr(
				"CASE WHEN "+table+".latest_success_at < ? THEN ? ELSE "+table+".latest_success_at END",
				bucket.LatestSuccessAt,
				bucket.LatestSuccessAt,
			),
			"latest_failure_at": gorm.Expr(
				"CASE WHEN "+table+".latest_failure_at < ? THEN ? ELSE "+table+".latest_failure_at END",
				bucket.LatestFailureAt,
				bucket.LatestFailureAt,
			),
		}),
	}).Create(bucket).Error
}

func GetContinuityRelayOutcomeBuckets(
	startTs int64,
	endTs int64,
) ([]ContinuityRelayOutcomeBucket, error) {
	var buckets []ContinuityRelayOutcomeBucket
	if endTs < startTs {
		return buckets, nil
	}
	err := DB.Where("bucket_ts >= ? AND bucket_ts <= ?", startTs, endTs).
		Order("bucket_ts ASC").
		Find(&buckets).Error
	return buckets, err
}

func GetContinuityRelayOutcomeBucketsForPair(
	groupKey string,
	modelId string,
	startTs int64,
	endTs int64,
) ([]ContinuityRelayOutcomeBucket, error) {
	var buckets []ContinuityRelayOutcomeBucket
	if groupKey == "" || modelId == "" || endTs < startTs {
		return buckets, nil
	}
	err := DB.Where(
		"group_key = ? AND model_id = ? AND bucket_ts >= ? AND bucket_ts <= ?",
		groupKey,
		modelId,
		startTs,
		endTs,
	).
		Order("bucket_ts ASC").
		Find(&buckets).Error
	return buckets, err
}

func DeleteContinuityRelayOutcomeBucketsBefore(cutoffTs int64) error {
	if cutoffTs <= 0 {
		return nil
	}
	return DB.Where("bucket_ts < ?", cutoffTs).
		Delete(&ContinuityRelayOutcomeBucket{}).Error
}
