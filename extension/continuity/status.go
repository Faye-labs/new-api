package continuity

import (
	"sort"
	"time"

	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
)

const (
	continuityGroupModelStatusSchemaVersion = 1
	continuityPassiveEvidenceWindowHours    = 24
	continuityStatusHistoryIntervalSeconds  = int64(20 * 60)
	continuityStatusHistoryPointLimit       = 72
	continuityStatusHistoryTaskLimit        = 2048
	continuityStatusMaxLatencyMs            = int64(time.Hour / time.Millisecond)

	continuityModelStatusOperational = "operational"
	continuityModelStatusDegraded    = "degraded"
	continuityModelStatusUnavailable = "unavailable"
	continuityModelStatusUnknown     = "unknown"

	continuityStatusSourceNone           = "none"
	continuityStatusSourcePassiveTraffic = "passive_traffic"
	continuityStatusSourceRealTraffic    = "real_traffic"
	continuityStatusSourceActiveProbe    = "active_probe"
)

type continuityGroupModelStatusSnapshot struct {
	SchemaVersion int                           `json:"schema_version"`
	GeneratedAt   int64                         `json:"generated_at"`
	Window        continuityStatusWindow        `json:"window"`
	Groups        []continuityRoutingGroupState `json:"groups"`
}

type continuityStatusWindow struct {
	StartAt int64 `json:"start_at"`
	EndAt   int64 `json:"end_at"`
}

type continuityRoutingGroupState struct {
	GroupKey string                        `json:"group_key"`
	Status   string                        `json:"status"`
	Models   []continuityRoutingModelState `json:"models"`
}

type continuityRoutingModelState struct {
	ModelID              string                             `json:"model_id"`
	EligibleChannelCount int                                `json:"eligible_channel_count"`
	Status               string                             `json:"status"`
	StatusSource         string                             `json:"status_source"`
	LatencyMs            *int64                             `json:"latency_ms,omitempty"`
	LastCheckedAt        *int64                             `json:"last_checked_at,omitempty"`
	History              continuityGroupModelStatusHistory  `json:"history"`
	Evidence             continuityGroupModelStatusEvidence `json:"evidence"`
}

type continuityGroupModelStatusHistory struct {
	WindowStartAt   int64                                    `json:"window_start_at"`
	WindowEndAt     int64                                    `json:"window_end_at"`
	IntervalSeconds int64                                    `json:"interval_seconds"`
	Points          []continuityGroupModelStatusHistoryPoint `json:"points"`
}

type continuityGroupModelStatusHistoryPoint struct {
	CheckedAt int64  `json:"checked_at"`
	Status    string `json:"status"`
	LatencyMs *int64 `json:"latency_ms,omitempty"`
}

type continuityGroupModelStatusEvidence struct {
	Passive     *continuityPassiveTrafficEvidence `json:"passive"`
	RealTraffic *continuityRealTrafficEvidence    `json:"real_traffic"`
	ActiveProbe *continuityActiveProbeEvidence    `json:"active_probe"`
}

type continuityPassiveTrafficEvidence struct {
	WindowStartAt    int64   `json:"window_start_at"`
	WindowEndAt      int64   `json:"window_end_at"`
	LatestBucketAt   int64   `json:"latest_bucket_at"`
	SuccessRate      float64 `json:"success_rate"`
	AverageLatencyMs int64   `json:"average_latency_ms"`
}

type continuityActiveProbeEvidence struct {
	Status    string `json:"status"`
	CheckedAt int64  `json:"checked_at"`
	LatencyMs int64  `json:"latency_ms"`
}

type continuityRealTrafficEvidence struct {
	ObservedAt int64 `json:"observed_at"`
	LatencyMs  int64 `json:"latency_ms"`
}

func groupModelStatusSnapshot(now time.Time) (continuityGroupModelStatusSnapshot, error) {
	now = now.UTC().Truncate(time.Second)
	windowStart := now.Add(-continuityPassiveEvidenceWindowHours * time.Hour)
	snapshot := continuityGroupModelStatusSnapshot{
		SchemaVersion: continuityGroupModelStatusSchemaVersion,
		GeneratedAt:   now.Unix(),
		Window: continuityStatusWindow{
			StartAt: windowStart.Unix(),
			EndAt:   now.Unix(),
		},
		Groups: make([]continuityRoutingGroupState, 0),
	}

	exclusions, err := loadContinuityGroupModelProbeExclusions()
	if err != nil {
		return continuityGroupModelStatusSnapshot{}, err
	}
	excludedPairs := continuityGroupModelProbeExclusionSet(exclusions)

	var abilities []model.Ability
	if err := model.DB.Where("enabled = ?", true).Find(&abilities).Error; err != nil {
		return continuityGroupModelStatusSnapshot{}, err
	}

	groupModels := make(map[string]map[string]map[int]struct{})
	modelNames := make(map[string]struct{})
	for _, ability := range abilities {
		pairKey := continuityGroupModelProbePairKey(ability.Group, ability.Model)
		if _, excluded := excludedPairs[pairKey]; excluded {
			continue
		}
		if _, ok := groupModels[ability.Group]; !ok {
			groupModels[ability.Group] = make(map[string]map[int]struct{})
		}
		if _, ok := groupModels[ability.Group][ability.Model]; !ok {
			groupModels[ability.Group][ability.Model] = make(map[int]struct{})
		}
		groupModels[ability.Group][ability.Model][ability.ChannelId] = struct{}{}
		modelNames[ability.Model] = struct{}{}
	}

	sortedModelNames := make([]string, 0, len(modelNames))
	for modelName := range modelNames {
		sortedModelNames = append(sortedModelNames, modelName)
	}
	sort.Strings(sortedModelNames)

	passiveByPair := make(map[string]map[string]perfmetrics.GroupResult)
	for _, modelName := range sortedModelNames {
		result, err := perfmetrics.Query(perfmetrics.QueryParams{
			Model: modelName,
			Hours: continuityPassiveEvidenceWindowHours,
		})
		if err != nil {
			return continuityGroupModelStatusSnapshot{}, err
		}
		for _, groupResult := range result.Groups {
			if _, configured := groupModels[groupResult.Group][modelName]; !configured {
				continue
			}
			if _, ok := passiveByPair[groupResult.Group]; !ok {
				passiveByPair[groupResult.Group] = make(map[string]perfmetrics.GroupResult)
			}
			passiveByPair[groupResult.Group][modelName] = groupResult
		}
	}

	activeByPair, err := latestContinuityGroupModelProbeEvidence(now)
	if err != nil {
		return continuityGroupModelStatusSnapshot{}, err
	}
	realTrafficMaxAge := continuityGroupModelRecentSuccessWindow
	recentTrafficByPair, err := latestPersistedContinuityRealTrafficEvidence(now, realTrafficMaxAge)
	if err != nil {
		return continuityGroupModelStatusSnapshot{}, err
	}
	for groupKey, models := range groupModels {
		for modelID := range models {
			if evidence, ok := latestContinuityRecentRelaySuccess(
				groupKey,
				modelID,
				now,
				realTrafficMaxAge,
			); ok {
				pairKey := continuityGroupModelProbePairKey(groupKey, modelID)
				persisted, exists := recentTrafficByPair[pairKey]
				if !exists || evidence.ObservedAt >= persisted.ObservedAt {
					recentTrafficByPair[pairKey] = evidence
				}
			}
		}
	}
	relayOutcomesByPair, err := loadContinuityRelayOutcomeEvidence(
		windowStart.Unix(),
		now.Unix(),
	)
	if err != nil {
		return continuityGroupModelStatusSnapshot{}, err
	}
	historyByPair, historyWindowStart, historyWindowEnd, err :=
		continuityGroupModelProbeHistory(now)
	if err != nil {
		return continuityGroupModelStatusSnapshot{}, err
	}
	historyByPair = mergeContinuityRelayOutcomeHistory(
		historyByPair,
		relayOutcomesByPair,
		historyWindowStart,
		historyWindowEnd,
	)

	groupKeys := make([]string, 0, len(groupModels))
	for groupKey := range groupModels {
		groupKeys = append(groupKeys, groupKey)
	}
	sort.Strings(groupKeys)

	for _, groupKey := range groupKeys {
		modelIDs := make([]string, 0, len(groupModels[groupKey]))
		for modelID := range groupModels[groupKey] {
			modelIDs = append(modelIDs, modelID)
		}
		sort.Strings(modelIDs)

		group := continuityRoutingGroupState{
			GroupKey: groupKey,
			Status:   continuityModelStatusUnknown,
			Models:   make([]continuityRoutingModelState, 0, len(modelIDs)),
		}
		for _, modelID := range modelIDs {
			pairKey := continuityGroupModelProbePairKey(groupKey, modelID)
			historyPoints := historyByPair[pairKey]
			state := continuityRoutingModelState{
				ModelID:              modelID,
				EligibleChannelCount: len(groupModels[groupKey][modelID]),
				Status:               continuityModelStatusUnknown,
				StatusSource:         continuityStatusSourceNone,
				History: continuityGroupModelStatusHistory{
					WindowStartAt:   historyWindowStart,
					WindowEndAt:     historyWindowEnd,
					IntervalSeconds: continuityStatusHistoryIntervalSeconds,
					Points: append(
						make(
							[]continuityGroupModelStatusHistoryPoint,
							0,
							len(historyPoints),
						),
						historyPoints...,
					),
				},
				Evidence: continuityGroupModelStatusEvidence{
					Passive:     nil,
					RealTraffic: nil,
					ActiveProbe: nil,
				},
			}

			passive, observed := passiveByPair[groupKey][modelID]
			if observed && len(passive.Series) > 0 {
				latestBucketAt := passive.Series[len(passive.Series)-1].Ts
				successRate := passive.SuccessRate
				latencyMs := continuityStatusLatencyMs(passive.AvgLatencyMs)

				state.Status = continuityModelStatusDegraded
				if successRate >= 100 {
					state.Status = continuityModelStatusOperational
				}
				state.StatusSource = continuityStatusSourcePassiveTraffic
				state.LatencyMs = &latencyMs
				state.LastCheckedAt = &latestBucketAt
				state.Evidence.Passive = &continuityPassiveTrafficEvidence{
					WindowStartAt:    windowStart.Unix(),
					WindowEndAt:      now.Unix(),
					LatestBucketAt:   latestBucketAt,
					SuccessRate:      successRate,
					AverageLatencyMs: latencyMs,
				}
			}

			active, activelyChecked := activeByPair[pairKey]
			if activelyChecked {
				activeLatencyMs := continuityStatusLatencyMs(active.LatencyMs)
				state.Evidence.ActiveProbe = &continuityActiveProbeEvidence{
					Status:    active.Status,
					CheckedAt: active.CheckedAt,
					LatencyMs: activeLatencyMs,
				}
				passiveMayBeNewer := false
				if state.Evidence.Passive != nil {
					// Perf metrics store only a bucket start, not the timestamp
					// of the latest sample inside it. Conservatively keep the
					// passive result whenever the probe happened before that
					// bucket closed, because traffic may have arrived later in
					// the same bucket.
					passiveBucketEnd := state.Evidence.Passive.LatestBucketAt +
						perf_metrics_setting.GetBucketSeconds()
					passiveMayBeNewer = active.CheckedAt < passiveBucketEnd
				}
				if active.Status != continuityModelStatusUnknown && !passiveMayBeNewer {
					state.Status = active.Status
					state.StatusSource = continuityStatusSourceActiveProbe
					state.LastCheckedAt = &active.CheckedAt
					state.LatencyMs = nil
					if activeLatencyMs > 0 {
						state.LatencyMs = &activeLatencyMs
					}
				} else if active.Status == continuityModelStatusUnknown &&
					state.StatusSource == continuityStatusSourceNone {
					state.StatusSource = continuityStatusSourceActiveProbe
					state.LastCheckedAt = &active.CheckedAt
				}
			}

			if traffic, observed := recentTrafficByPair[pairKey]; observed {
				trafficLatencyMs := continuityStatusLatencyMs(traffic.LatencyMs)
				state.Evidence.RealTraffic = &continuityRealTrafficEvidence{
					ObservedAt: traffic.ObservedAt,
					LatencyMs:  trafficLatencyMs,
				}
				realTrafficIsCurrent := !activelyChecked ||
					active.Status == continuityModelStatusUnknown ||
					traffic.ObservedAt >= active.CheckedAt
				if realTrafficIsCurrent {
					state.Status = continuityModelStatusOperational
					state.StatusSource = continuityStatusSourceRealTraffic
					state.LastCheckedAt = &traffic.ObservedAt
					state.LatencyMs = &trafficLatencyMs
				}
			}

			if trafficStatus, checkedAt, latencyMs, observed :=
				continuityCurrentUserTrafficStatus(relayOutcomesByPair[pairKey], now); observed {
				trafficOverrides := trafficStatus != continuityModelStatusOperational ||
					!activelyChecked || checkedAt >= active.CheckedAt
				if trafficOverrides {
					state.Status = trafficStatus
					state.StatusSource = continuityStatusSourceRealTraffic
					state.LastCheckedAt = &checkedAt
					state.LatencyMs = nil
					if latencyMs > 0 {
						latency := continuityStatusLatencyMs(latencyMs)
						state.LatencyMs = &latency
					}
					state.Evidence.RealTraffic = &continuityRealTrafficEvidence{
						ObservedAt: checkedAt,
						LatencyMs:  continuityStatusLatencyMs(latencyMs),
					}
				}
			}
			group.Models = append(group.Models, state)
		}
		group.Status = continuityAggregateGroupStatus(group.Models)
		snapshot.Groups = append(snapshot.Groups, group)
	}

	return snapshot, nil
}

func continuityGroupModelProbeHistory(
	now time.Time,
) (map[string][]continuityGroupModelStatusHistoryPoint, int64, int64, error) {
	windowEnd := now.UTC().Truncate(time.Second).Unix()
	windowStart := windowEnd - int64(continuityStatusHistoryPointLimit)*continuityStatusHistoryIntervalSeconds

	var tasks []model.SystemTask
	if err := model.DB.
		Select("id", "result").
		Where("type = ? AND status = ?", continuityGroupModelProbeTaskType, model.SystemTaskStatusSucceeded).
		Where("updated_at >= ?", windowStart).
		Order("id DESC").
		Limit(continuityStatusHistoryTaskLimit).
		Find(&tasks).Error; err != nil {
		return nil, 0, 0, err
	}

	bucketsByPair := make(map[string]map[int]continuityGroupModelStatusHistoryPoint)
	for _, task := range tasks {
		result, err := decodeContinuityGroupModelProbeResult(task.Result)
		if err != nil {
			// Historical observability is best-effort. A malformed older task
			// must not suppress the current status snapshot or newer checks.
			continue
		}
		if result.SchemaVersion != continuityGroupModelStatusSchemaVersion {
			continue
		}
		for _, evidence := range result.Pairs {
			if evidence.CheckedAt < windowStart || evidence.CheckedAt > windowEnd {
				continue
			}
			switch evidence.Status {
			case continuityModelStatusOperational,
				continuityModelStatusDegraded,
				continuityModelStatusUnavailable,
				continuityModelStatusUnknown:
			default:
				continue
			}

			bucketIndex := int(
				(evidence.CheckedAt - windowStart) / continuityStatusHistoryIntervalSeconds,
			)
			if bucketIndex >= continuityStatusHistoryPointLimit {
				bucketIndex = continuityStatusHistoryPointLimit - 1
			}
			pairKey := continuityGroupModelProbePairKey(evidence.GroupKey, evidence.ModelID)
			if _, ok := bucketsByPair[pairKey]; !ok {
				bucketsByPair[pairKey] = make(map[int]continuityGroupModelStatusHistoryPoint)
			}
			current, exists := bucketsByPair[pairKey][bucketIndex]
			if exists && current.CheckedAt >= evidence.CheckedAt {
				continue
			}
			point := continuityGroupModelStatusHistoryPoint{
				CheckedAt: evidence.CheckedAt,
				Status:    evidence.Status,
			}
			if evidence.LatencyMs > 0 {
				latencyMs := continuityStatusLatencyMs(evidence.LatencyMs)
				point.LatencyMs = &latencyMs
			}
			bucketsByPair[pairKey][bucketIndex] = point
		}
	}

	historyByPair := make(map[string][]continuityGroupModelStatusHistoryPoint, len(bucketsByPair))
	for pairKey, buckets := range bucketsByPair {
		bucketIndexes := make([]int, 0, len(buckets))
		for bucketIndex := range buckets {
			bucketIndexes = append(bucketIndexes, bucketIndex)
		}
		sort.Ints(bucketIndexes)
		points := make([]continuityGroupModelStatusHistoryPoint, 0, len(bucketIndexes))
		for _, bucketIndex := range bucketIndexes {
			points = append(points, buckets[bucketIndex])
		}
		historyByPair[pairKey] = points
	}

	return historyByPair, windowStart, windowEnd, nil
}

func continuityStatusLatencyMs(latencyMs int64) int64 {
	if latencyMs < 0 {
		return 0
	}
	if latencyMs > continuityStatusMaxLatencyMs {
		return continuityStatusMaxLatencyMs
	}
	return latencyMs
}

func continuityAggregateGroupStatus(models []continuityRoutingModelState) string {
	if len(models) == 0 {
		return continuityModelStatusUnknown
	}

	statusCounts := make(map[string]int)
	for _, modelState := range models {
		statusCounts[modelState.Status]++
	}
	if statusCounts[continuityModelStatusUnavailable] == len(models) {
		return continuityModelStatusUnavailable
	}
	if statusCounts[continuityModelStatusOperational] == len(models) {
		return continuityModelStatusOperational
	}
	if statusCounts[continuityModelStatusUnknown] == len(models) {
		return continuityModelStatusUnknown
	}
	if statusCounts[continuityModelStatusDegraded] > 0 ||
		statusCounts[continuityModelStatusUnavailable] > 0 {
		return continuityModelStatusDegraded
	}
	return continuityModelStatusUnknown
}
