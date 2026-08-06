package continuity

import (
	"container/list"
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
	continuityRecentRelaySuccesses.Lock()
	originalRecentRelaySuccesses := continuityRecentRelaySuccesses.byPair
	originalRecentRelaySuccessOrder := continuityRecentRelaySuccesses.order
	originalRecentRelaySuccessCleanup := continuityRecentRelaySuccesses.lastCleanupAt
	continuityRecentRelaySuccesses.byPair = make(map[string]*continuityRecentRelaySuccessEntry)
	continuityRecentRelaySuccesses.order = list.New()
	continuityRecentRelaySuccesses.lastCleanupAt = 0
	continuityRecentRelaySuccesses.Unlock()
	common.OptionMapRWMutex.Lock()
	originalOptionMap := common.OptionMap
	common.OptionMap = make(map[string]string)
	common.OptionMapRWMutex.Unlock()

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
		continuityRecentRelaySuccesses.Lock()
		continuityRecentRelaySuccesses.byPair = originalRecentRelaySuccesses
		continuityRecentRelaySuccesses.order = originalRecentRelaySuccessOrder
		continuityRecentRelaySuccesses.lastCleanupAt = originalRecentRelaySuccessCleanup
		continuityRecentRelaySuccesses.Unlock()
		common.OptionMapRWMutex.Lock()
		common.OptionMap = originalOptionMap
		common.OptionMapRWMutex.Unlock()
		sqlDB, dbErr := database.DB()
		if dbErr == nil {
			require.NoError(t, sqlDB.Close())
		}
	})
	require.NoError(t, database.AutoMigrate(
		&model.User{},
		&model.Token{},
		&model.Channel{},
		&model.Ability{},
		&model.Option{},
		&model.SystemTask{},
		&model.SystemTaskLock{},
		&model.ContinuityRelayOutcomeBucket{},
	))
	return database
}

func TestContinuityGroupModelProbeExclusionsPersistAsExactSortedPairs(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(
		`{"direct":1,"standard":0.24}`,
	))

	saved, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{
			{GroupKey: "standard", ModelID: "model-z"},
			{GroupKey: "direct", ModelID: "model-a"},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []continuityGroupModelProbeExclusion{
		{GroupKey: "direct", ModelID: "model-a"},
		{GroupKey: "standard", ModelID: "model-z"},
	}, saved)

	loaded, err := loadContinuityGroupModelProbeExclusions()
	require.NoError(t, err)
	assert.Equal(t, saved, loaded)
}

func TestContinuityGroupModelProbeExclusionsRemainReadableAfterGroupRemoval(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"standard":1}`))

	saved, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{{
			GroupKey: "standard",
			ModelID:  "compat-model",
		}},
	)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"direct":1}`))

	loaded, err := loadContinuityGroupModelProbeExclusions()
	require.NoError(t, err)
	assert.Equal(t, saved, loaded)
}

func TestContinuityGroupModelProbeExclusionsCanPreserveButNotIntroduceStalePairs(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(
		`{"direct":1,"standard":1}`,
	))
	_, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{
			{GroupKey: "direct", ModelID: "model-old"},
			{GroupKey: "standard", ModelID: "model-stale"},
		},
	)
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"direct":1}`))

	updated, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{
			{GroupKey: "direct", ModelID: "model-new"},
			{GroupKey: "standard", ModelID: "model-stale"},
		},
	)
	require.NoError(t, err)
	assert.Equal(t, []continuityGroupModelProbeExclusion{
		{GroupKey: "direct", ModelID: "model-new"},
		{GroupKey: "standard", ModelID: "model-stale"},
	}, updated)

	_, err = replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{
			{GroupKey: "direct", ModelID: "model-new"},
			{GroupKey: "missing", ModelID: "model-newly-stale"},
			{GroupKey: "standard", ModelID: "model-stale"},
		},
	)
	require.ErrorIs(t, err, errUnknownRoutingGroup)

	loaded, err := loadContinuityGroupModelProbeExclusions()
	require.NoError(t, err)
	assert.Equal(t, updated, loaded)
}

func TestContinuityGroupModelProbeExclusionsSurfacePersistenceFailure(t *testing.T) {
	database := setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"standard":1}`))
	require.NoError(t, database.Migrator().DropTable(&model.Option{}))

	_, err := replaceContinuityGroupModelProbeExclusions(
		[]continuityGroupModelProbeExclusion{{
			GroupKey: "standard",
			ModelID:  "compat-model",
		}},
	)
	require.Error(t, err)
}

func TestContinuityGroupModelProbeExclusionsRejectAmbiguousPairs(t *testing.T) {
	setupContinuityManagedGroupServiceTest(t)
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"standard":1}`))

	for _, pairs := range [][]continuityGroupModelProbeExclusion{
		{
			{GroupKey: "standard", ModelID: "model-a"},
			{GroupKey: "standard", ModelID: "model-a"},
		},
		{{GroupKey: "missing", ModelID: "model-a"}},
		{{GroupKey: "standard", ModelID: " model-a"}},
		{{GroupKey: "standard", ModelID: "model-\ninvalid"}},
	} {
		_, err := replaceContinuityGroupModelProbeExclusions(pairs)
		require.Error(t, err)
	}
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
