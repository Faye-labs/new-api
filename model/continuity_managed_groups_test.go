package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupContinuityManagedGroupsDB(t *testing.T) *gorm.DB {
	t.Helper()
	originalDB := DB
	originalRedisEnabled := common.RedisEnabled
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, originalLogDatabaseType)
	common.RedisEnabled = false

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	DB = database

	t.Cleanup(func() {
		DB = originalDB
		common.RedisEnabled = originalRedisEnabled
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		sqlDB, dbErr := database.DB()
		if dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	require.NoError(t, database.AutoMigrate(&User{}, &Token{}, &Ability{}))
	return database
}

func TestUpdateContinuityManagedUserGroupIsIdempotent(t *testing.T) {
	database := setupContinuityManagedGroupsDB(t)
	user := User{
		Id:       7,
		Username: "managed-user",
		Password: "password",
		Status:   common.UserStatusEnabled,
		Group:    "free",
	}
	require.NoError(t, database.Create(&user).Error)

	changed, err := UpdateContinuityManagedUserGroup(user.Id, "pulse")
	require.NoError(t, err)
	assert.True(t, changed)

	changed, err = UpdateContinuityManagedUserGroup(user.Id, "pulse")
	require.NoError(t, err)
	assert.False(t, changed)

	var stored User
	require.NoError(t, database.First(&stored, user.Id).Error)
	assert.Equal(t, "pulse", stored.Group)
}

func TestUpdateContinuityManagedTokenGroupsIsAtomicAndOwnerBound(t *testing.T) {
	database := setupContinuityManagedGroupsDB(t)
	tokens := []Token{
		{Id: 11, UserId: 7, Key: "token-eleven", Status: common.TokenStatusEnabled, Group: "old"},
		{Id: 12, UserId: 8, Key: "token-twelve", Status: common.TokenStatusEnabled, Group: "old"},
	}
	require.NoError(t, database.Create(&tokens).Error)

	_, err := UpdateContinuityManagedTokenGroups([]ContinuityManagedTokenGroupUpdate{
		{TokenID: 11, UserID: 7, Group: "route-clay"},
		{TokenID: 12, UserID: 7, Group: "route-clay"},
	})
	require.ErrorIs(t, err, ErrContinuityManagedTokenOwnerMismatch)

	var stored []Token
	require.NoError(t, database.Order("id").Find(&stored).Error)
	assert.Equal(t, "old", stored[0].Group)
	assert.Equal(t, "old", stored[1].Group)
}

func TestUpdateContinuityManagedTokenGroupsRejectsDisabledToken(t *testing.T) {
	database := setupContinuityManagedGroupsDB(t)
	token := Token{
		Id:     21,
		UserId: 7,
		Key:    "token-disabled",
		Status: common.TokenStatusDisabled,
		Group:  "old",
	}
	require.NoError(t, database.Create(&token).Error)

	_, err := UpdateContinuityManagedTokenGroups([]ContinuityManagedTokenGroupUpdate{
		{TokenID: token.Id, UserID: token.UserId, Group: "route-clay"},
	})
	require.ErrorIs(t, err, ErrContinuityManagedTokenDisabled)

	var stored Token
	require.NoError(t, database.First(&stored, token.Id).Error)
	assert.Equal(t, "old", stored.Group)
}

func TestGetContinuityRoutingGroupDetailsReturnsSortedEnabledModels(t *testing.T) {
	database := setupContinuityManagedGroupsDB(t)
	abilities := []Ability{
		{Group: "route-clay", Model: "model-z", ChannelId: 1, Enabled: true},
		{Group: "route-clay", Model: "model-a", ChannelId: 1, Enabled: true},
		{Group: "route-clay", Model: "model-a", ChannelId: 2, Enabled: true},
		{Group: "route-clay", Model: "model-hidden", ChannelId: 3, Enabled: false},
	}
	require.NoError(t, database.Create(&abilities).Error)

	models, channels, err := GetContinuityRoutingGroupDetails("route-clay")
	require.NoError(t, err)
	assert.Equal(t, []string{"model-a", "model-z"}, models)
	assert.Equal(t, int64(2), channels)
}
