package continuity

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

const (
	continuityGroupModelProbeExclusionsOptionKey = "continuity.group_model_probe.exclusions"
	continuityGroupModelProbeExclusionsSchema    = 1
	continuityGroupModelProbeExclusionsMaxPairs  = 512
	continuityGroupModelProbeModelMaxBytes       = 255
)

type continuityGroupModelProbeExclusion struct {
	GroupKey string `json:"group_key"`
	ModelID  string `json:"model_id"`
}

type continuityGroupModelProbeExclusions struct {
	SchemaVersion int                                  `json:"schema_version"`
	Pairs         []continuityGroupModelProbeExclusion `json:"pairs"`
}

type continuityGroupModelProbeExclusionState struct {
	Initialized bool
	Pairs       []continuityGroupModelProbeExclusion
}

func canonicalizeContinuityGroupModelProbeExclusions(
	pairs []continuityGroupModelProbeExclusion,
) ([]continuityGroupModelProbeExclusion, error) {
	if len(pairs) > continuityGroupModelProbeExclusionsMaxPairs {
		return nil, errInvalidRequest
	}

	normalized := make([]continuityGroupModelProbeExclusion, 0, len(pairs))
	seen := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		groupKey, err := normalizeContinuityManagedGroup(pair.GroupKey)
		if err != nil {
			return nil, errInvalidRequest
		}

		modelID := pair.ModelID
		if modelID == "" ||
			!utf8.ValidString(modelID) ||
			modelID != strings.TrimSpace(modelID) ||
			len(modelID) > continuityGroupModelProbeModelMaxBytes {
			return nil, errInvalidRequest
		}
		for _, character := range modelID {
			if unicode.IsControl(character) {
				return nil, errInvalidRequest
			}
		}

		key := continuityGroupModelProbePairKey(groupKey, modelID)
		if _, exists := seen[key]; exists {
			return nil, errInvalidRequest
		}
		seen[key] = struct{}{}
		normalized = append(normalized, continuityGroupModelProbeExclusion{
			GroupKey: groupKey,
			ModelID:  modelID,
		})
	}

	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].GroupKey != normalized[j].GroupKey {
			return normalized[i].GroupKey < normalized[j].GroupKey
		}
		return normalized[i].ModelID < normalized[j].ModelID
	})
	return normalized, nil
}

func validateContinuityGroupModelProbeExclusionGroups(
	pairs []continuityGroupModelProbeExclusion,
	persisted []continuityGroupModelProbeExclusion,
) error {
	persistedPairs := continuityGroupModelProbeExclusionSet(persisted)
	for _, pair := range pairs {
		if ratio_setting.ContainsGroupRatio(pair.GroupKey) {
			continue
		}
		pairKey := continuityGroupModelProbePairKey(pair.GroupKey, pair.ModelID)
		if _, alreadyPersisted := persistedPairs[pairKey]; !alreadyPersisted {
			return fmt.Errorf(
				"%w: group=%s",
				errUnknownRoutingGroup,
				pair.GroupKey,
			)
		}
	}
	return nil
}

func loadContinuityGroupModelProbeExclusionState() (
	continuityGroupModelProbeExclusionState,
	error,
) {
	var option model.Option
	result := model.DB.
		Where(&model.Option{Key: continuityGroupModelProbeExclusionsOptionKey}).
		Limit(1).
		Find(&option)
	if result.Error != nil {
		return continuityGroupModelProbeExclusionState{}, result.Error
	}
	if result.RowsAffected == 0 {
		return continuityGroupModelProbeExclusionState{
			Initialized: false,
			Pairs:       []continuityGroupModelProbeExclusion{},
		}, nil
	}

	var stored continuityGroupModelProbeExclusions
	if err := common.UnmarshalJsonStr(option.Value, &stored); err != nil {
		return continuityGroupModelProbeExclusionState{}, err
	}
	if stored.SchemaVersion != continuityGroupModelProbeExclusionsSchema {
		return continuityGroupModelProbeExclusionState{}, fmt.Errorf(
			"unsupported Continuity probe exclusion schema: %d",
			stored.SchemaVersion,
		)
	}
	pairs, err := canonicalizeContinuityGroupModelProbeExclusions(stored.Pairs)
	if err != nil {
		return continuityGroupModelProbeExclusionState{}, err
	}
	return continuityGroupModelProbeExclusionState{
		Initialized: true,
		Pairs:       pairs,
	}, nil
}

func loadContinuityGroupModelProbeExclusions() (
	[]continuityGroupModelProbeExclusion,
	error,
) {
	state, err := loadContinuityGroupModelProbeExclusionState()
	if err != nil {
		return nil, err
	}
	return state.Pairs, nil
}

func replaceContinuityGroupModelProbeExclusions(
	pairs []continuityGroupModelProbeExclusion,
) ([]continuityGroupModelProbeExclusion, error) {
	normalized, err := canonicalizeContinuityGroupModelProbeExclusions(pairs)
	if err != nil {
		return nil, err
	}
	persistedState, err := loadContinuityGroupModelProbeExclusionState()
	if err != nil {
		return nil, err
	}
	if err := validateContinuityGroupModelProbeExclusionGroups(
		normalized,
		persistedState.Pairs,
	); err != nil {
		return nil, err
	}
	encoded, err := common.Marshal(continuityGroupModelProbeExclusions{
		SchemaVersion: continuityGroupModelProbeExclusionsSchema,
		Pairs:         normalized,
	})
	if err != nil {
		return nil, err
	}
	if err := model.UpdateOptionsBulk(map[string]string{
		continuityGroupModelProbeExclusionsOptionKey: string(encoded),
	}); err != nil {
		return nil, err
	}

	persisted, err := loadContinuityGroupModelProbeExclusions()
	if err != nil {
		return nil, err
	}
	if len(persisted) != len(normalized) {
		return nil, fmt.Errorf("Continuity probe exclusions did not persist")
	}
	for index := range normalized {
		if persisted[index] != normalized[index] {
			return nil, fmt.Errorf("Continuity probe exclusions changed while persisting")
		}
	}
	return persisted, nil
}

func continuityGroupModelProbeExclusionSet(
	pairs []continuityGroupModelProbeExclusion,
) map[string]struct{} {
	excluded := make(map[string]struct{}, len(pairs))
	for _, pair := range pairs {
		excluded[continuityGroupModelProbePairKey(pair.GroupKey, pair.ModelID)] =
			struct{}{}
	}
	return excluded
}
