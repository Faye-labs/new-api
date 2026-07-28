package model

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
)

var (
	ErrContinuityManagedUserNotFound          = errors.New("continuity managed user not found")
	ErrContinuityManagedTokenNotFound         = errors.New("continuity managed token not found")
	ErrContinuityManagedTokenDisabled         = errors.New("continuity managed token is not enabled")
	ErrContinuityManagedTokenOwnerMismatch    = errors.New("continuity managed token owner mismatch")
	ErrContinuityManagedTokenIdentityMismatch = errors.New("continuity managed token identity mismatch")
)

const continuityAccountAPITokenName = "continuity-account-api-managed"

// IsContinuityAccountAPIToken reports whether a token carries the fixed
// Account API domain marker. The caller must still bind the exact token id and
// user id; the name alone never establishes ownership.
func IsContinuityAccountAPIToken(token *Token) bool {
	return token != nil && token.Name == continuityAccountAPITokenName
}

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

// DisableContinuityAccountAPIToken disables and soft-deletes one exact hidden
// Account API token. The durable user/token identity is checked before the
// fixed domain name, and the cache population fence is advanced synchronously
// after the database commit so a concurrent stale read cannot repopulate it.
func DisableContinuityAccountAPIToken(userID int, tokenID int) (bool, error) {
	changed := false
	var tokenKey string
	err := DB.Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx.Unscoped()).Where("id = ?", tokenID).First(&token).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("%w: token_id=%d", ErrContinuityManagedTokenNotFound, tokenID)
			}
			return err
		}
		if token.UserId != userID {
			return fmt.Errorf(
				"%w: token_id=%d expected_user_id=%d",
				ErrContinuityManagedTokenOwnerMismatch,
				tokenID,
				userID,
			)
		}
		if token.Name != continuityAccountAPITokenName {
			return fmt.Errorf(
				"%w: token_id=%d",
				ErrContinuityManagedTokenIdentityMismatch,
				tokenID,
			)
		}
		tokenKey = token.Key
		if token.Status == common.TokenStatusDisabled && token.DeletedAt.Valid {
			return nil
		}

		result := tx.Unscoped().Model(&Token{}).
			Where("id = ? AND user_id = ? AND name = ?", tokenID, userID, continuityAccountAPITokenName).
			Updates(map[string]interface{}{
				"status":     common.TokenStatusDisabled,
				"deleted_at": time.Now(),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return fmt.Errorf("continuity managed Account API token changed during disable: token_id=%d", tokenID)
		}
		changed = true

		var disabled Token
		if err := tx.Unscoped().Where("id = ? AND user_id = ?", tokenID, userID).First(&disabled).Error; err != nil {
			return err
		}
		if disabled.Name != continuityAccountAPITokenName ||
			disabled.Status != common.TokenStatusDisabled ||
			!disabled.DeletedAt.Valid {
			return fmt.Errorf("continuity managed Account API token did not become inactive: token_id=%d", tokenID)
		}
		return nil
	})
	if err != nil {
		return false, err
	}

	cacheErr := invalidateCachesAfterMutation(func() error {
		if !common.RedisEnabled || tokenKey == "" {
			return nil
		}
		return cacheDeleteToken(tokenKey)
	})
	if cacheErr != nil {
		return changed, fmt.Errorf("continuity managed Account API token disabled but cache invalidation failed: %w", cacheErr)
	}
	return changed, nil
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
