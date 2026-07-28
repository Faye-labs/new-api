package continuity

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/model"
	newapiservice "github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
)

const (
	continuityManagedGroupMaxLength = 64
	continuityManagedTokenBatchMax  = 100
)

var (
	errInvalidRequest      = errors.New("invalid continuity managed group request")
	errUnknownRoutingGroup = errors.New("unknown continuity routing group")
)

type managedTokenGroupUpdate struct {
	TokenID int
	UserID  int
	Group   string
}

type managedTokenGroupResult struct {
	TokenID int    `json:"token_id"`
	UserID  int    `json:"user_id"`
	Group   string `json:"group"`
	Changed bool   `json:"changed"`
}

type accountAPITokenDisableResult struct {
	TokenID int  `json:"token_id"`
	UserID  int  `json:"user_id"`
	Changed bool `json:"changed"`
}

type ContinuityRoutingGroup struct {
	Key                string   `json:"key"`
	Description        string   `json:"description"`
	Ratio              float64  `json:"ratio"`
	ModelCount         int64    `json:"model_count"`
	ChannelCount       int64    `json:"channel_count"`
	Models             []string `json:"models"`
	UsableByUserGroups []string `json:"usable_by_user_groups"`
}

func normalizeContinuityManagedGroup(group string) (string, error) {
	if group == "" ||
		!utf8.ValidString(group) ||
		group != strings.TrimSpace(group) ||
		len(group) > continuityManagedGroupMaxLength {
		return "", errInvalidRequest
	}
	for _, character := range group {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return "", errInvalidRequest
		}
	}
	return group, nil
}

func updateManagedUserGroup(userID int, group string) (string, bool, error) {
	group, err := normalizeContinuityManagedGroup(group)
	if userID <= 0 || err != nil {
		return "", false, errInvalidRequest
	}
	if !ratio_setting.ContainsGroupRatio(group) {
		return "", false, fmt.Errorf(
			"%w: group=%s",
			errUnknownRoutingGroup,
			group,
		)
	}

	changed, err := model.UpdateContinuityManagedUserGroup(userID, group)
	return group, changed, err
}

func updateManagedTokenGroups(updates []managedTokenGroupUpdate) ([]managedTokenGroupResult, error) {
	if len(updates) == 0 || len(updates) > continuityManagedTokenBatchMax {
		return nil, errInvalidRequest
	}

	seenTokenIDs := make(map[int]struct{}, len(updates))
	modelUpdates := make([]model.ContinuityManagedTokenGroupUpdate, 0, len(updates))
	for _, update := range updates {
		group, err := normalizeContinuityManagedGroup(update.Group)
		if update.TokenID <= 0 || update.UserID <= 0 || err != nil {
			return nil, errInvalidRequest
		}
		if _, exists := seenTokenIDs[update.TokenID]; exists {
			return nil, fmt.Errorf("%w: duplicate token_id=%d", errInvalidRequest, update.TokenID)
		}
		seenTokenIDs[update.TokenID] = struct{}{}
		if !ratio_setting.ContainsGroupRatio(group) {
			return nil, fmt.Errorf("%w: group=%s", errUnknownRoutingGroup, group)
		}
		modelUpdates = append(modelUpdates, model.ContinuityManagedTokenGroupUpdate{
			TokenID: update.TokenID,
			UserID:  update.UserID,
			Group:   group,
		})
	}

	modelResults, err := model.UpdateContinuityManagedTokenGroups(modelUpdates)
	if err != nil {
		return nil, err
	}
	results := make([]managedTokenGroupResult, 0, len(modelResults))
	for _, result := range modelResults {
		results = append(results, managedTokenGroupResult{
			TokenID: result.TokenID,
			UserID:  result.UserID,
			Group:   result.Group,
			Changed: result.Changed,
		})
	}
	return results, nil
}

func disableAccountAPIToken(userID int, tokenID int) (accountAPITokenDisableResult, error) {
	if userID <= 0 || tokenID <= 0 {
		return accountAPITokenDisableResult{}, errInvalidRequest
	}
	changed, err := model.DisableContinuityAccountAPIToken(userID, tokenID)
	if err != nil {
		return accountAPITokenDisableResult{}, err
	}
	return accountAPITokenDisableResult{
		TokenID: tokenID,
		UserID:  userID,
		Changed: changed,
	}, nil
}

func listRoutingGroups() ([]ContinuityRoutingGroup, error) {
	groupRatios := ratio_setting.GetGroupRatioCopy()
	keys := make([]string, 0, len(groupRatios))
	for key := range groupRatios {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	groups := make([]ContinuityRoutingGroup, 0, len(keys))
	for _, key := range keys {
		models, channelCount, err := model.GetContinuityRoutingGroupDetails(key)
		if err != nil {
			return nil, err
		}
		usableByUserGroups := make([]string, 0, 3)
		for _, userGroup := range []string{"free", "pulse", "lux"} {
			if newapiservice.GroupInUserUsableGroups(userGroup, key) {
				usableByUserGroups = append(usableByUserGroups, userGroup)
			}
		}
		sort.Strings(usableByUserGroups)
		groups = append(groups, ContinuityRoutingGroup{
			Key:                key,
			Description:        setting.GetUsableGroupDescription(key),
			Ratio:              groupRatios[key],
			ModelCount:         int64(len(models)),
			ChannelCount:       channelCount,
			Models:             models,
			UsableByUserGroups: usableByUserGroups,
		})
	}
	return groups, nil
}
