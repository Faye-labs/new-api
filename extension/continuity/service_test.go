package continuity

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupContinuityManagedGroupServiceTest(t *testing.T) *gorm.DB {
	t.Helper()

	originalDB := model.DB
	originalRedisEnabled := common.RedisEnabled
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	originalUserUsableGroups := setting.UserUsableGroups2JSONString()
	originalSpecialUsableGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup.ReadAll()

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, originalLogDatabaseType)
	common.RedisEnabled = false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	database, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	model.DB = database

	t.Cleanup(func() {
		model.DB = originalDB
		common.RedisEnabled = originalRedisEnabled
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUserUsableGroups))
		specialGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
		specialGroups.Clear()
		specialGroups.AddAll(originalSpecialUsableGroups)
		sqlDB, dbErr := database.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Token{}, &model.Ability{}))
	return database
}

func TestUpdateContinuityManagedTokenGroupsRequiresOwnerAndExactGroupKey(t *testing.T) {
	invalidUpdates := []managedTokenGroupUpdate{
		{TokenID: 1, UserID: 0, Group: "route-clay"},
		{TokenID: 1, UserID: 7, Group: " route-clay"},
		{TokenID: 1, UserID: 7, Group: "route clay"},
		{TokenID: 1, UserID: 7, Group: strings.Repeat("a", continuityManagedGroupMaxLength+1)},
	}
	for _, update := range invalidUpdates {
		_, err := updateManagedTokenGroups([]managedTokenGroupUpdate{update})
		require.ErrorIs(t, err, errInvalidRequest)
	}
}

func TestUpdateContinuityManagedUserGroupRejectsUnknownGroupBeforeMutation(t *testing.T) {
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
	})
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"free":1}`))

	_, _, err := updateManagedUserGroup(7, "missing")
	require.ErrorIs(t, err, errUnknownRoutingGroup)
}

func TestListContinuityRoutingGroupsReturnsModelsAndTierUsability(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)

	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"route-clay":1,"route-luxury":2}`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(
		`{"route-clay":"Clay","route-luxury":"Luxury"}`,
	))
	specialGroups := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup
	specialGroups.Clear()
	specialGroups.AddAll(map[string]map[string]string{
		"free":  {"-:route-luxury": ""},
		"pulse": {},
		"lux":   {},
	})

	require.NoError(t, database.Create(&[]model.Ability{
		{Group: "route-luxury", Model: "model-z", ChannelId: 10, Enabled: true},
		{Group: "route-luxury", Model: "model-a", ChannelId: 10, Enabled: true},
		{Group: "route-luxury", Model: "model-a", ChannelId: 11, Enabled: true},
		{Group: "route-luxury", Model: "model-hidden", ChannelId: 12, Enabled: false},
	}).Error)

	groups, err := listRoutingGroups()
	require.NoError(t, err)
	require.Len(t, groups, 2)
	assert.Equal(t, "route-clay", groups[0].Key)
	assert.Equal(t, []string{"free", "lux", "pulse"}, groups[0].UsableByUserGroups)
	assert.Equal(t, "route-luxury", groups[1].Key)
	assert.Equal(t, "Luxury", groups[1].Description)
	assert.Equal(t, float64(2), groups[1].Ratio)
	assert.Equal(t, []string{"model-a", "model-z"}, groups[1].Models)
	assert.Equal(t, int64(2), groups[1].ModelCount)
	assert.Equal(t, int64(2), groups[1].ChannelCount)
	assert.Equal(t, []string{"lux", "pulse"}, groups[1].UsableByUserGroups)
}
