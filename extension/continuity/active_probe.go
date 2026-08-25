package continuity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"

	"gorm.io/gorm"
)

const (
	continuityGroupModelProbeTaskType = "continuity_group_model_probe"

	continuityGroupModelProbeEnabledEnv         = "CONTINUITY_GROUP_MODEL_PROBE_ENABLED"
	continuityGroupModelProbeIntervalMinutesEnv = "CONTINUITY_GROUP_MODEL_PROBE_INTERVAL_MINUTES"

	continuityGroupModelProbeDefaultIntervalMinutes = 20
	continuityGroupModelProbeManualCooldown         = 60 * time.Second
	continuityGroupModelProbeConfirmationDelay      = 60 * time.Second
	continuityGroupModelProbeEvidenceMaxAge         = 45 * time.Minute
	continuityGroupModelProbeAttemptTimeout         = 45 * time.Second
)

type continuityGroupModelProbeHandler struct{}

type continuityGroupModelProbePayload struct {
	Manual bool `json:"manual,omitempty"`
}

type continuityGroupModelProbeResult struct {
	SchemaVersion int                                 `json:"schema_version"`
	CheckedAt     int64                               `json:"checked_at"`
	Summary       continuityGroupModelProbeSummary    `json:"summary"`
	Pairs         []continuityGroupModelProbeEvidence `json:"pairs"`
}

type continuityGroupModelProbeSummary struct {
	Total            int `json:"total"`
	Operational      int `json:"operational"`
	Degraded         int `json:"degraded"`
	Unavailable      int `json:"unavailable"`
	Unknown          int `json:"unknown"`
	TrafficCovered   int `json:"traffic_covered,omitempty"`
	Probed           int `json:"probed,omitempty"`
	ProviderAttempts int `json:"provider_attempts,omitempty"`
}

type continuityGroupModelProbeEvidence struct {
	GroupKey         string `json:"group_key"`
	ModelID          string `json:"model_id"`
	Status           string `json:"status"`
	Source           string `json:"source,omitempty"`
	CheckedAt        int64  `json:"checked_at"`
	LatencyMs        int64  `json:"latency_ms"`
	NextRotation     int    `json:"next_rotation"`
	ProbeAttempted   bool   `json:"probe_attempted,omitempty"`
	ProviderAttempts int    `json:"provider_attempts,omitempty"`
}

type continuityGroupModelProbeTaskView struct {
	TaskID    string                            `json:"task_id"`
	Status    model.SystemTaskStatus            `json:"status"`
	Active    bool                              `json:"active"`
	Created   *bool                             `json:"created,omitempty"`
	CreatedAt int64                             `json:"created_at"`
	UpdatedAt int64                             `json:"updated_at"`
	Progress  service.SystemTaskProgress        `json:"progress"`
	Summary   *continuityGroupModelProbeSummary `json:"summary,omitempty"`
}

type continuityGroupModelProbeCandidate struct {
	channel  *model.Channel
	priority int64
}

type continuityGroupModelProbePair struct {
	groupKey         string
	modelID          string
	candidates       []continuityGroupModelProbeCandidate
	nextRotation     int
	probeAttempted   bool
	providerAttempts int
}

type continuityGroupModelProbeRoundResult struct {
	status           string
	latencyMs        int64
	checkedAt        int64
	needsConfirm     bool
	source           string
	probeAttempted   bool
	trafficCovered   bool
	providerAttempts int
}

var (
	runContinuityChannelProbe = controller.ProbeChannel
	waitForContinuityProbe    = waitForContinuityProbeConfirmation
	continuityProbeNow        = time.Now
)

func (continuityGroupModelProbeHandler) Type() string {
	return continuityGroupModelProbeTaskType
}

func (continuityGroupModelProbeHandler) Enabled() bool {
	if strings.TrimSpace(os.Getenv(ContinuityInternalAPISecretEnv)) == "" {
		return false
	}
	return common.GetEnvOrDefaultBool(continuityGroupModelProbeEnabledEnv, true)
}

func (continuityGroupModelProbeHandler) Interval() time.Duration {
	minutes := common.GetEnvOrDefault(
		continuityGroupModelProbeIntervalMinutesEnv,
		continuityGroupModelProbeDefaultIntervalMinutes,
	)
	if minutes < 1 {
		minutes = continuityGroupModelProbeDefaultIntervalMinutes
	}
	return time.Duration(minutes) * time.Minute
}

func (continuityGroupModelProbeHandler) NewPayload() any {
	return continuityGroupModelProbePayload{}
}

func (continuityGroupModelProbeHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	result, err := runContinuityGroupModelProbe(
		ctx,
		task,
		service.NewSystemTaskProgressReporter(task, runnerID),
	)
	status := model.SystemTaskStatusSucceeded
	errorMessage := ""
	if err != nil {
		status = model.SystemTaskStatusFailed
		errorMessage = err.Error()
		result = continuityGroupModelProbeResult{}
	}
	if finishErr := model.FinishSystemTask(task.TaskID, runnerID, status, result, errorMessage); finishErr != nil {
		common.SysLog(fmt.Sprintf("continuity group model probe task %s failed to persist result: %v", task.TaskID, finishErr))
	}
}

func runContinuityGroupModelProbe(
	ctx context.Context,
	task *model.SystemTask,
	report func(processed, total int),
) (continuityGroupModelProbeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pairs, err := loadContinuityGroupModelProbePairs()
	if err != nil {
		return continuityGroupModelProbeResult{}, err
	}
	previous, err := loadPreviousContinuityGroupModelProbeResult(task)
	if err != nil {
		return continuityGroupModelProbeResult{}, err
	}
	payload := continuityGroupModelProbePayload{}
	if task != nil {
		if err := task.DecodePayload(&payload); err != nil {
			return continuityGroupModelProbeResult{}, err
		}
	}
	trafficCoverageMaxAge := continuityUserTrafficWindow

	evidenceByPair := make(map[string]continuityGroupModelProbeEvidence, len(pairs))
	probedPairs := make(map[string]struct{}, len(pairs))
	providerAttemptsTotal := 0
	needsConfirmation := make([]continuityGroupModelProbePair, 0)
	processed := 0
	total := len(pairs)
	if report != nil {
		report(0, total)
	}

	for index := range pairs {
		if err := ctx.Err(); err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		pair := &pairs[index]
		pairKey := continuityGroupModelProbePairKey(pair.groupKey, pair.modelID)
		if previousEvidence, ok := previous[pairKey]; ok {
			pair.nextRotation = previousEvidence.NextRotation
		}
		excluded, err := isContinuityGroupModelProbePairExcluded(pair.groupKey, pair.modelID)
		if err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		if excluded {
			processed++
			if report != nil {
				report(processed, total)
			}
			continue
		}
		if traffic, covered := currentContinuityGroupModelTraffic(
			*pair,
			continuityProbeNow(),
			trafficCoverageMaxAge,
		); covered {
			evidenceByPair[pairKey] = continuityGroupModelProbeEvidence{
				GroupKey:     pair.groupKey,
				ModelID:      pair.modelID,
				Status:       traffic.status,
				Source:       traffic.source,
				CheckedAt:    traffic.checkedAt,
				LatencyMs:    traffic.latencyMs,
				NextRotation: pair.nextRotation,
			}
			processed++
			if report != nil {
				report(processed, total)
			}
			continue
		}
		startingRotation := pair.nextRotation
		orderedCandidates, nextRotation := rotateContinuityGroupModelProbeCandidates(
			pair.candidates,
			pair.nextRotation,
		)
		pair.candidates = orderedCandidates
		pair.nextRotation = nextRotation

		round, excluded, err := probeContinuityGroupModelPair(
			ctx,
			*pair,
			trafficCoverageMaxAge,
		)
		if err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		providerAttemptsTotal += round.providerAttempts
		if round.probeAttempted {
			probedPairs[pairKey] = struct{}{}
		}
		if err := ctx.Err(); err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		if excluded {
			processed++
			if report != nil {
				report(processed, total)
			}
			continue
		}
		if !round.probeAttempted {
			pair.nextRotation = startingRotation
		}
		if round.needsConfirm {
			pair.probeAttempted = round.probeAttempted
			pair.providerAttempts = round.providerAttempts
			needsConfirmation = append(needsConfirmation, *pair)
			continue
		}
		evidenceByPair[pairKey] = continuityGroupModelProbeEvidence{
			GroupKey:         pair.groupKey,
			ModelID:          pair.modelID,
			Status:           round.status,
			Source:           round.source,
			CheckedAt:        round.checkedAt,
			LatencyMs:        round.latencyMs,
			NextRotation:     pair.nextRotation,
			ProbeAttempted:   round.probeAttempted,
			ProviderAttempts: round.providerAttempts,
		}
		processed++
		if report != nil {
			report(processed, total)
		}
	}

	if len(needsConfirmation) > 0 {
		if err := waitForContinuityProbe(ctx, continuityGroupModelProbeConfirmationDelay); err != nil {
			return continuityGroupModelProbeResult{}, err
		}
	}
	for _, pair := range needsConfirmation {
		if err := ctx.Err(); err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		pairKey := continuityGroupModelProbePairKey(pair.groupKey, pair.modelID)
		round, excluded, err := probeContinuityGroupModelPair(
			ctx,
			pair,
			trafficCoverageMaxAge,
		)
		if err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		providerAttemptsTotal += round.providerAttempts
		if round.probeAttempted {
			probedPairs[pairKey] = struct{}{}
		}
		if err := ctx.Err(); err != nil {
			return continuityGroupModelProbeResult{}, err
		}
		if excluded {
			processed++
			if report != nil {
				report(processed, total)
			}
			continue
		}
		if round.needsConfirm {
			round.status = continuityModelStatusUnavailable
		} else if !round.trafficCovered && (round.status == continuityModelStatusOperational ||
			round.status == continuityModelStatusDegraded) {
			// A pair that failed its entire first pass but recovered during
			// confirmation remains degraded for this snapshot.
			round.status = continuityModelStatusDegraded
		}
		evidenceByPair[pairKey] = continuityGroupModelProbeEvidence{
			GroupKey:         pair.groupKey,
			ModelID:          pair.modelID,
			Status:           round.status,
			Source:           round.source,
			CheckedAt:        round.checkedAt,
			LatencyMs:        round.latencyMs,
			NextRotation:     pair.nextRotation,
			ProbeAttempted:   pair.probeAttempted || round.probeAttempted,
			ProviderAttempts: pair.providerAttempts + round.providerAttempts,
		}
		processed++
		if report != nil {
			report(processed, total)
		}
	}

	result := continuityGroupModelProbeResult{
		SchemaVersion: 1,
		CheckedAt:     continuityProbeNow().UTC().Unix(),
		Pairs:         make([]continuityGroupModelProbeEvidence, 0, len(pairs)),
	}
	for _, pair := range pairs {
		evidence, ok := evidenceByPair[continuityGroupModelProbePairKey(pair.groupKey, pair.modelID)]
		if !ok {
			continue
		}
		result.Pairs = append(result.Pairs, evidence)
		switch evidence.Status {
		case continuityModelStatusOperational:
			result.Summary.Operational++
		case continuityModelStatusDegraded:
			result.Summary.Degraded++
		case continuityModelStatusUnavailable:
			result.Summary.Unavailable++
		default:
			result.Summary.Unknown++
		}
		if evidence.Source == continuityStatusSourceRealTraffic {
			result.Summary.TrafficCovered++
		}
	}
	result.Summary.Total = len(result.Pairs)
	result.Summary.Probed = len(probedPairs)
	result.Summary.ProviderAttempts = providerAttemptsTotal
	if report != nil {
		report(total, total)
	}
	return result, nil
}

func loadContinuityGroupModelProbePairs() ([]continuityGroupModelProbePair, error) {
	exclusionState, err := loadContinuityGroupModelProbeExclusionState()
	if err != nil {
		return nil, err
	}
	if !exclusionState.Initialized {
		return []continuityGroupModelProbePair{}, nil
	}
	excludedPairs := continuityGroupModelProbeExclusionSet(exclusionState.Pairs)

	var abilities []model.Ability
	if err := model.DB.Where("enabled = ?", true).Find(&abilities).Error; err != nil {
		return nil, err
	}

	channelIDs := make([]int, 0, len(abilities))
	seenChannelIDs := make(map[int]struct{}, len(abilities))
	for _, ability := range abilities {
		if _, ok := seenChannelIDs[ability.ChannelId]; ok {
			continue
		}
		seenChannelIDs[ability.ChannelId] = struct{}{}
		channelIDs = append(channelIDs, ability.ChannelId)
	}
	channelsByID := make(map[int]*model.Channel, len(channelIDs))
	if len(channelIDs) > 0 {
		var channels []*model.Channel
		if err := model.DB.
			Where("id IN ? AND status = ?", channelIDs, common.ChannelStatusEnabled).
			Find(&channels).Error; err != nil {
			return nil, err
		}
		for _, channel := range channels {
			channelsByID[channel.Id] = channel
		}
	}

	pairsByKey := make(map[string]*continuityGroupModelProbePair)
	for _, ability := range abilities {
		pairKey := continuityGroupModelProbePairKey(ability.Group, ability.Model)
		if _, excluded := excludedPairs[pairKey]; excluded {
			continue
		}
		pair, ok := pairsByKey[pairKey]
		if !ok {
			pair = &continuityGroupModelProbePair{
				groupKey: ability.Group,
				modelID:  ability.Model,
			}
			pairsByKey[pairKey] = pair
		}
		channel := channelsByID[ability.ChannelId]
		if channel == nil {
			continue
		}
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		pair.candidates = append(pair.candidates, continuityGroupModelProbeCandidate{
			channel:  channel,
			priority: priority,
		})
	}

	pairs := make([]continuityGroupModelProbePair, 0, len(pairsByKey))
	for _, pair := range pairsByKey {
		sort.Slice(pair.candidates, func(i, j int) bool {
			if pair.candidates[i].priority != pair.candidates[j].priority {
				return pair.candidates[i].priority > pair.candidates[j].priority
			}
			return pair.candidates[i].channel.Id < pair.candidates[j].channel.Id
		})
		pairs = append(pairs, *pair)
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].groupKey != pairs[j].groupKey {
			return pairs[i].groupKey < pairs[j].groupKey
		}
		return pairs[i].modelID < pairs[j].modelID
	})
	return pairs, nil
}

func rotateContinuityGroupModelProbeCandidates(
	candidates []continuityGroupModelProbeCandidate,
	rotation int,
) ([]continuityGroupModelProbeCandidate, int) {
	if len(candidates) < 2 {
		return candidates, 0
	}
	topPriority := candidates[0].priority
	topCount := 1
	for topCount < len(candidates) && candidates[topCount].priority == topPriority {
		topCount++
	}
	if topCount < 2 {
		return candidates, 0
	}
	if rotation < 0 {
		rotation = 0
	}
	rotation %= topCount
	rotated := make([]continuityGroupModelProbeCandidate, 0, len(candidates))
	rotated = append(rotated, candidates[rotation:topCount]...)
	rotated = append(rotated, candidates[:rotation]...)
	rotated = append(rotated, candidates[topCount:]...)
	return rotated, (rotation + 1) % topCount
}

func probeContinuityGroupModelPair(
	ctx context.Context,
	pair continuityGroupModelProbePair,
	trafficCoverageMaxAge time.Duration,
) (continuityGroupModelProbeRoundResult, bool, error) {
	failed := 0
	uncertain := 0
	probeAttempted := false
	providerAttempts := 0
	for _, candidate := range pair.candidates {
		excluded, err := isContinuityGroupModelProbePairExcluded(
			pair.groupKey,
			pair.modelID,
		)
		if err != nil {
			return continuityGroupModelProbeRoundResult{}, false, err
		}
		if excluded {
			return continuityGroupModelProbeRoundResult{
				probeAttempted:   probeAttempted,
				providerAttempts: providerAttempts,
			}, true, nil
		}
		now := continuityProbeNow()
		traffic, covered := currentContinuityGroupModelTraffic(
			pair,
			now,
			trafficCoverageMaxAge,
		)
		if covered {
			traffic.probeAttempted = probeAttempted
			traffic.providerAttempts = providerAttempts
			return traffic, false, nil
		}
		attemptContext, cancelAttempt := context.WithTimeout(
			ctx,
			continuityGroupModelProbeAttemptTimeout,
		)
		result := runContinuityChannelProbe(
			attemptContext,
			candidate.channel,
			pair.modelID,
			pair.groupKey,
		)
		probeAttempted = true
		providerAttempts++
		cancelAttempt()
		checkedAt := continuityProbeNow().UTC().Unix()
		switch result.Status {
		case controller.ChannelProbeStatusSucceeded:
			status := continuityModelStatusOperational
			if failed > 0 {
				status = continuityModelStatusDegraded
			}
			return continuityGroupModelProbeRoundResult{
				status:           status,
				latencyMs:        result.LatencyMs,
				checkedAt:        checkedAt,
				source:           continuityStatusSourceActiveProbe,
				probeAttempted:   probeAttempted,
				providerAttempts: providerAttempts,
			}, false, nil
		case controller.ChannelProbeStatusFailed:
			failed++
		case controller.ChannelProbeStatusCancelled:
			if ctx.Err() == nil {
				failed++
				continue
			}
			return continuityGroupModelProbeRoundResult{
				status:           continuityModelStatusUnknown,
				checkedAt:        checkedAt,
				source:           continuityStatusSourceActiveProbe,
				probeAttempted:   probeAttempted,
				providerAttempts: providerAttempts,
			}, false, nil
		case controller.ChannelProbeStatusUnsupported,
			controller.ChannelProbeStatusIndeterminate:
			uncertain++
		default:
			uncertain++
		}
	}
	checkedAt := continuityProbeNow().UTC().Unix()
	if failed == 0 || uncertain > 0 {
		return continuityGroupModelProbeRoundResult{
			status:           continuityModelStatusUnknown,
			checkedAt:        checkedAt,
			source:           continuityStatusSourceActiveProbe,
			probeAttempted:   probeAttempted,
			providerAttempts: providerAttempts,
		}, false, nil
	}
	return continuityGroupModelProbeRoundResult{
		status:           continuityModelStatusUnknown,
		checkedAt:        checkedAt,
		needsConfirm:     true,
		source:           continuityStatusSourceActiveProbe,
		probeAttempted:   probeAttempted,
		providerAttempts: providerAttempts,
	}, false, nil
}

func currentContinuityGroupModelTraffic(
	pair continuityGroupModelProbePair,
	now time.Time,
	maxAge time.Duration,
) (continuityGroupModelProbeRoundResult, bool) {
	outcome, ok := latestContinuityRecentRelayOutcome(
		pair.groupKey,
		pair.modelID,
		now,
		maxAge,
	)
	if !ok {
		return continuityGroupModelProbeRoundResult{}, false
	}
	status, checkedAt, latencyMs, observed := continuityCurrentUserTrafficStatus(
		&continuityRelayOutcomePairEvidence{
			LatestSuccessAt:        outcome.LatestSuccessAt,
			LatestSuccessLatencyMs: outcome.LatestSuccessLatencyMs,
			LatestFailureAt:        outcome.LatestFailureAt,
		},
		now,
	)
	if !observed {
		return continuityGroupModelProbeRoundResult{}, false
	}
	return continuityGroupModelProbeRoundResult{
		status:         status,
		latencyMs:      latencyMs,
		checkedAt:      checkedAt,
		source:         continuityStatusSourceRealTraffic,
		trafficCovered: true,
	}, true
}

func isContinuityGroupModelProbePairExcluded(groupKey string, modelID string) (bool, error) {
	exclusionState, err := loadContinuityGroupModelProbeExclusionState()
	if err != nil {
		return false, err
	}
	if !exclusionState.Initialized {
		return true, nil
	}
	pairKey := continuityGroupModelProbePairKey(groupKey, modelID)
	_, excluded := continuityGroupModelProbeExclusionSet(exclusionState.Pairs)[pairKey]
	return excluded, nil
}

func waitForContinuityProbeConfirmation(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func loadPreviousContinuityGroupModelProbeResult(
	currentTask *model.SystemTask,
) (map[string]continuityGroupModelProbeEvidence, error) {
	resultByPair := make(map[string]continuityGroupModelProbeEvidence)
	query := model.DB.
		Where("type = ? AND status = ?", continuityGroupModelProbeTaskType, model.SystemTaskStatusSucceeded).
		Order("id desc")
	if currentTask != nil && currentTask.ID > 0 {
		query = query.Where("id < ?", currentTask.ID)
	}
	var previousTask model.SystemTask
	if err := query.First(&previousTask).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return resultByPair, nil
		}
		return nil, err
	}
	previousResult, err := decodeContinuityGroupModelProbeResult(previousTask.Result)
	if err != nil {
		return nil, err
	}
	for _, evidence := range previousResult.Pairs {
		resultByPair[continuityGroupModelProbePairKey(evidence.GroupKey, evidence.ModelID)] = evidence
	}
	return resultByPair, nil
}

func latestContinuityGroupModelProbeEvidence(
	now time.Time,
) (map[string]continuityGroupModelProbeEvidence, error) {
	evidenceByPair := make(map[string]continuityGroupModelProbeEvidence)
	var latestTask model.SystemTask
	if err := model.DB.
		Where("type = ? AND status = ?", continuityGroupModelProbeTaskType, model.SystemTaskStatusSucceeded).
		Order("id desc").
		First(&latestTask).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return evidenceByPair, nil
		}
		return nil, err
	}
	result, err := decodeContinuityGroupModelProbeResult(latestTask.Result)
	if err != nil {
		return nil, err
	}
	cutoff := now.UTC().Add(-continuityGroupModelProbeEvidenceMaxAge).Unix()
	for _, evidence := range result.Pairs {
		if evidence.Source != "" && evidence.Source != continuityStatusSourceActiveProbe {
			continue
		}
		if evidence.CheckedAt < cutoff {
			continue
		}
		evidenceByPair[continuityGroupModelProbePairKey(evidence.GroupKey, evidence.ModelID)] = evidence
	}
	return evidenceByPair, nil
}

func latestPersistedContinuityRealTrafficEvidence(
	now time.Time,
	maxAge time.Duration,
) (map[string]continuityRecentRelaySuccess, error) {
	evidenceByPair := make(map[string]continuityRecentRelaySuccess)
	cutoff := now.UTC().Add(-maxAge).Unix()
	var tasks []model.SystemTask
	if err := model.DB.
		Select("id", "result").
		Where("type = ? AND status = ?", continuityGroupModelProbeTaskType, model.SystemTaskStatusSucceeded).
		Where("updated_at >= ?", cutoff).
		Order("id DESC").
		Limit(continuityStatusHistoryTaskLimit).
		Find(&tasks).Error; err != nil {
		return nil, err
	}
	for _, task := range tasks {
		result, err := decodeContinuityGroupModelProbeResult(task.Result)
		if err != nil {
			continue
		}
		for _, evidence := range result.Pairs {
			if evidence.Source != continuityStatusSourceRealTraffic ||
				evidence.Status != continuityModelStatusOperational ||
				evidence.CheckedAt < cutoff || evidence.CheckedAt > now.UTC().Unix() {
				continue
			}
			pairKey := continuityGroupModelProbePairKey(evidence.GroupKey, evidence.ModelID)
			current, exists := evidenceByPair[pairKey]
			if exists && current.ObservedAt >= evidence.CheckedAt {
				continue
			}
			evidenceByPair[pairKey] = continuityRecentRelaySuccess{
				ObservedAt: evidence.CheckedAt,
				LatencyMs:  evidence.LatencyMs,
			}
		}
	}
	return evidenceByPair, nil
}

func decodeContinuityGroupModelProbeResult(raw string) (continuityGroupModelProbeResult, error) {
	result := continuityGroupModelProbeResult{}
	if strings.TrimSpace(raw) == "" {
		return result, nil
	}
	if err := common.UnmarshalJsonStr(raw, &result); err != nil {
		return continuityGroupModelProbeResult{}, err
	}
	return result, nil
}

func continuityGroupModelProbePairKey(groupKey string, modelID string) string {
	return groupKey + "\x00" + modelID
}

func enqueueContinuityGroupModelProbe() (*model.SystemTask, bool, error) {
	activeTask, err := model.GetActiveSystemTask(continuityGroupModelProbeTaskType)
	if err != nil {
		return nil, false, err
	}
	if activeTask != nil {
		return activeTask, false, nil
	}
	latestTask, err := model.GetLatestSystemTask(continuityGroupModelProbeTaskType)
	if err != nil {
		return nil, false, err
	}
	if latestTask != nil &&
		common.GetTimestamp()-latestTask.UpdatedAt < int64(continuityGroupModelProbeManualCooldown.Seconds()) {
		return latestTask, false, nil
	}
	return service.EnqueueSystemTask(
		continuityGroupModelProbeTaskType,
		continuityGroupModelProbePayload{Manual: true},
	)
}

func currentOrLatestContinuityGroupModelProbeTask() (*model.SystemTask, error) {
	activeTask, err := model.GetActiveSystemTask(continuityGroupModelProbeTaskType)
	if err != nil || activeTask != nil {
		return activeTask, err
	}
	return model.GetLatestSystemTask(continuityGroupModelProbeTaskType)
}

func buildContinuityGroupModelProbeTaskView(
	task *model.SystemTask,
) (continuityGroupModelProbeTaskView, error) {
	view := continuityGroupModelProbeTaskView{
		TaskID:    task.TaskID,
		Status:    task.Status,
		Active:    task.Status == model.SystemTaskStatusPending || task.Status == model.SystemTaskStatusRunning,
		CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}
	if err := task.DecodeState(&view.Progress); err != nil {
		return continuityGroupModelProbeTaskView{}, err
	}
	if task.Status == model.SystemTaskStatusSucceeded && strings.TrimSpace(task.Result) != "" {
		result, err := decodeContinuityGroupModelProbeResult(task.Result)
		if err != nil {
			return continuityGroupModelProbeTaskView{}, err
		}
		view.Summary = &result.Summary
		if view.Progress.Total == 0 {
			view.Progress = service.SystemTaskProgress{
				Total:     result.Summary.Total,
				Processed: result.Summary.Total,
				Progress:  100,
			}
		}
	}
	return view, nil
}
