package model

import (
	"errors"
	"fmt"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

var (
	ErrContinuityManagedUserNotFound       = errors.New("continuity managed user not found")
	ErrContinuityManagedTokenNotFound      = errors.New("continuity managed token not found")
	ErrContinuityManagedTokenDisabled      = errors.New("continuity managed token is not enabled")
	ErrContinuityManagedTokenOwnerMismatch = errors.New("continuity managed token owner mismatch")
)

type ContinuityManagedTokenGroupUpdate struct {
	TokenID int
	UserID  int
	Group   string
}

type ContinuityManagedTokenGroupResult struct {
	TokenID int
	UserID  int
	Group   string
	Changed bool
}

func UpdateContinuityManagedUserGroup(userID int, group string) (bool, error) {
	changed := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Where("id = ?", userID).First(&user).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: user_id=%d", ErrContinuityManagedUserNotFound, userID)
			}
			return err
		}
		if user.Group == group {
			return nil
		}
		if err := tx.Model(&User{}).Where("id = ?", userID).Update("group", group).Error; err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}

	cacheErr := invalidateCachesAfterMutation(func() error {
		userCacheErr := InvalidateUserCache(userID)
		tokenCacheErr := InvalidateUserTokensCache(userID)
		return errors.Join(userCacheErr, tokenCacheErr)
	})
	if cacheErr != nil {
		return changed, fmt.Errorf("continuity managed user group committed but cache invalidation failed: %w", cacheErr)
	}
	return changed, nil
}

func UpdateContinuityManagedTokenGroups(updates []ContinuityManagedTokenGroupUpdate) ([]ContinuityManagedTokenGroupResult, error) {
	results := make([]ContinuityManagedTokenGroupResult, 0, len(updates))
	tokensByID := make(map[int]Token, len(updates))

	err := DB.Transaction(func(tx *gorm.DB) error {
		ids := make([]int, 0, len(updates))
		for _, update := range updates {
			ids = append(ids, update.TokenID)
		}

		var tokens []Token
		if err := lockForUpdate(tx).Where("id IN ?", ids).Find(&tokens).Error; err != nil {
			return err
		}
		for _, token := range tokens {
			tokensByID[token.Id] = token
		}

		for _, update := range updates {
			token, ok := tokensByID[update.TokenID]
			if !ok {
				return fmt.Errorf("%w: token_id=%d", ErrContinuityManagedTokenNotFound, update.TokenID)
			}
			if token.Status != common.TokenStatusEnabled {
				return fmt.Errorf("%w: token_id=%d", ErrContinuityManagedTokenDisabled, update.TokenID)
			}
			if token.UserId != update.UserID {
				return fmt.Errorf(
					"%w: token_id=%d expected_user_id=%d",
					ErrContinuityManagedTokenOwnerMismatch,
					update.TokenID,
					update.UserID,
				)
			}

			changed := token.Group != update.Group
			if changed {
				if err := tx.Model(&Token{}).
					Where("id = ?", update.TokenID).
					Update("group", update.Group).Error; err != nil {
					return err
				}
				token.Group = update.Group
				tokensByID[update.TokenID] = token
			}
			results = append(results, ContinuityManagedTokenGroupResult{
				TokenID: update.TokenID,
				UserID:  token.UserId,
				Group:   update.Group,
				Changed: changed,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if common.RedisEnabled {
		cacheErr := invalidateCachesAfterMutation(func() error {
			var invalidateErr error
			for _, update := range updates {
				token := tokensByID[update.TokenID]
				if err := cacheDeleteToken(token.Key); err != nil {
					invalidateErr = errors.Join(invalidateErr, err)
				}
			}
			return invalidateErr
		})
		if cacheErr != nil {
			return results, fmt.Errorf("continuity managed token groups committed but cache invalidation failed: %w", cacheErr)
		}
	}
	return results, nil
}

func GetContinuityRoutingGroupDetails(group string) ([]string, int64, error) {
	models := make([]string, 0)
	if err := DB.Model(&Ability{}).
		Where(&Ability{Group: group, Enabled: true}).
		Distinct("model").
		Order("model ASC").
		Pluck("model", &models).Error; err != nil {
		return nil, 0, err
	}

	var channelCount int64
	if err := DB.Model(&Ability{}).
		Where(&Ability{Group: group, Enabled: true}).
		Distinct("channel_id").
		Count(&channelCount).Error; err != nil {
		return nil, 0, err
	}
	return models, channelCount, nil
}
