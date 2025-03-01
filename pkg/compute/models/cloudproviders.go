// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package models

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"yunion.io/x/cloudmux/pkg/cloudprovider"
	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/pkg/tristate"
	"yunion.io/x/pkg/util/compare"
	"yunion.io/x/pkg/util/rbacscope"
	"yunion.io/x/pkg/util/timeutils"
	"yunion.io/x/pkg/utils"
	"yunion.io/x/sqlchemy"

	"yunion.io/x/onecloud/pkg/apis"
	api "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/cloudcommon/db"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/lockman"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/proxy"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/cloudcommon/validators"
	"yunion.io/x/onecloud/pkg/compute/options"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/mcclient"
	"yunion.io/x/onecloud/pkg/mcclient/auth"
	"yunion.io/x/onecloud/pkg/mcclient/modules/identity"
	"yunion.io/x/onecloud/pkg/util/logclient"
	"yunion.io/x/onecloud/pkg/util/stringutils2"
)

type SCloudproviderManager struct {
	db.SEnabledStatusStandaloneResourceBaseManager
	db.SProjectizedResourceBaseManager

	SProjectMappingResourceBaseManager
	SSyncableBaseResourceManager
}

var CloudproviderManager *SCloudproviderManager

func init() {
	CloudproviderManager = &SCloudproviderManager{
		SEnabledStatusStandaloneResourceBaseManager: db.NewEnabledStatusStandaloneResourceBaseManager(
			SCloudprovider{},
			"cloudproviders_tbl",
			"cloudprovider",
			"cloudproviders",
		),
	}
	CloudproviderManager.SetVirtualObject(CloudproviderManager)
}

type SCloudprovider struct {
	db.SEnabledStatusStandaloneResourceBase
	db.SProjectizedResourceBase

	SSyncableBaseResource

	// 云端服务健康状态。例如欠费、项目冻结都属于不健康状态。
	//
	// | HealthStatus  | 说明                 |
	// |---------------|----------------------|
	// | normal        | 远端处于健康状态     |
	// | insufficient  | 不足按需资源余额     |
	// | suspended     | 远端处于冻结状态     |
	// | arrears       | 远端处于欠费状态     |
	// | unknown       | 未知状态，查询失败   |
	// | no permission | 没有权限获取账单信息 |
	//
	HealthStatus string `width:"16" charset:"ascii" default:"normal" nullable:"false" list:"domain"`

	// Hostname string `width:"64" charset:"ascii" nullable:"true"` // Column(VARCHAR(64, charset='ascii'), nullable=False)
	// port = Column(Integer, nullable=False)
	// Version string `width:"32" charset:"ascii" nullable:"true" list:"domain"` // Column(VARCHAR(32, charset='ascii'), nullable=True)
	// Sysinfo jsonutils.JSONObject `get:"domain"` // Column(JSONEncodedDict, nullable=True)

	AccessUrl string `width:"64" charset:"ascii" nullable:"true" list:"domain" update:"domain" create:"domain_optional"`
	// 云账号的用户信息，例如用户名，access key等
	Account string `width:"128" charset:"ascii" nullable:"false" list:"domain" create:"domain_required"`
	// 云账号的密码信息，例如密码，access key secret等。该字段在数据库加密存储。Google需要存储秘钥证书,需要此字段比较长
	Secret string `length:"0" charset:"ascii" nullable:"false" list:"domain" create:"domain_required"`

	// 归属云账号ID
	CloudaccountId string `width:"36" charset:"ascii" nullable:"false" list:"user" create:"required"`

	// ProjectId string `name:"tenant_id" width:"128" charset:"ascii" nullable:"true" list:"domain"`

	// LastSync time.Time `get:"domain" list:"domain"` // = Column(DateTime, nullable=True)

	// 云账号的平台信息
	Provider string `width:"64" charset:"ascii" list:"domain" create:"domain_required"`

	SProjectMappingResourceBase
}

type pmCache struct {
	Id                      string
	CloudaccountId          string
	AccountProjectMappingId string
	ManagerProjectMappingId string

	AccountEnableProjectSync bool
	ManagerEnableProjectSync bool

	AccountEnableResourceSync bool
	ManagerEnableResourceSync bool
}

type sProjectMapping struct {
	*SProjectMapping
	EnableProjectSync  bool
	EnableResourceSync bool
}

func (self *sProjectMapping) IsNeedResourceSync() bool {
	return self.EnableResourceSync || !self.EnableProjectSync
}

func (self *sProjectMapping) IsNeedProjectSync() bool {
	return self.EnableProjectSync
}

func (self *pmCache) GetProjectMapping() (*sProjectMapping, error) {
	if len(self.ManagerProjectMappingId) > 0 {
		pm, err := GetRuleMapping(self.ManagerProjectMappingId)
		if err != nil {
			return nil, errors.Wrapf(err, "GetRuleMapping(%s)", self.ManagerProjectMappingId)
		}
		ret := &sProjectMapping{
			SProjectMapping:    pm,
			EnableProjectSync:  self.ManagerEnableProjectSync,
			EnableResourceSync: self.ManagerEnableResourceSync,
		}
		return ret, nil
	}
	if len(self.AccountProjectMappingId) > 0 {
		ret := &sProjectMapping{
			EnableProjectSync:  self.AccountEnableProjectSync,
			EnableResourceSync: self.AccountEnableResourceSync,
		}
		var err error
		ret.SProjectMapping, err = GetRuleMapping(self.AccountProjectMappingId)
		return ret, err
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, "empty project mapping id")
}

var pmCaches map[string]*pmCache = map[string]*pmCache{}

func refreshPmCaches() error {
	q := CloudproviderManager.Query().SubQuery()
	providers := q.Query(
		q.Field("cloudaccount_id"),
		q.Field("id"),
		q.Field("project_mapping_id").Label("manager_project_mapping_id"),
		q.Field("enable_project_sync").Label("manager_enable_project_sync"),
		q.Field("enable_resource_sync").Label("manager_enable_resource_sync"),
	)
	sq := CloudaccountManager.Query().SubQuery()
	mq := providers.LeftJoin(sq, sqlchemy.Equals(q.Field("cloudaccount_id"), sq.Field("id"))).
		AppendField(sq.Field("project_mapping_id").Label("account_project_mapping_id")).
		AppendField(sq.Field("enable_project_sync").Label("account_enable_project_sync")).
		AppendField(sq.Field("enable_resource_sync").Label("account_enable_resource_sync"))
	caches := []pmCache{}
	err := mq.All(&caches)
	if err != nil {
		return errors.Wrapf(err, "q.All")
	}
	for i := range caches {
		pmCaches[caches[i].Id] = &caches[i]
	}
	return nil
}

func (self *SCloudaccount) GetProjectMapping() (*sProjectMapping, error) {
	cache, err := func() (*pmCache, error) {
		for id := range pmCaches {
			if pmCaches[id].CloudaccountId == self.Id {
				return pmCaches[id], nil
			}
		}
		err := refreshPmCaches()
		if err != nil {
			return nil, errors.Wrapf(err, "refreshPmCaches")
		}
		for id := range pmCaches {
			if pmCaches[id].CloudaccountId == self.Id {
				return pmCaches[id], nil
			}
		}
		return nil, cloudprovider.ErrNotFound
	}()
	if err != nil {
		return nil, errors.Wrapf(err, "get project mapping cache")
	}
	return cache.GetProjectMapping()
}

func (self *SCloudprovider) GetProjectMapping() (*sProjectMapping, error) {
	cache, err := func() (*pmCache, error) {
		mp, ok := pmCaches[self.Id]
		if ok {
			return mp, nil
		}
		err := refreshPmCaches()
		if err != nil {
			return nil, errors.Wrapf(err, "refreshPmCaches")
		}
		return pmCaches[self.Id], nil
	}()
	if err != nil {
		return nil, errors.Wrapf(err, "get project mapping cache")
	}
	return cache.GetProjectMapping()
}

func (self *SCloudprovider) ValidateDeleteCondition(ctx context.Context, info jsonutils.JSONObject) error {
	if self.GetEnabled() {
		return httperrors.NewInvalidStatusError("provider is enabled")
	}
	if self.SyncStatus != api.CLOUD_PROVIDER_SYNC_STATUS_IDLE {
		return httperrors.NewInvalidStatusError("provider is not idle")
	}
	return self.SEnabledStatusStandaloneResourceBase.ValidateDeleteCondition(ctx, nil)
}

func (manager *SCloudproviderManager) GetPublicProviderIdsQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderIdsQuery(tristate.True, tristate.None, nil, nil)
}

func (manager *SCloudproviderManager) GetPrivateProviderIdsQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderIdsQuery(tristate.False, tristate.False, nil, nil)
}

func (manager *SCloudproviderManager) GetOnPremiseProviderIdsQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderIdsQuery(tristate.None, tristate.True, nil, nil)
}

func (manager *SCloudproviderManager) GetPrivateOrOnPremiseProviderIdsQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderIdsQuery(tristate.False, tristate.None, nil, nil)
}

func (manager *SCloudproviderManager) GetProviderIdsQuery(isPublic tristate.TriState, isOnPremise tristate.TriState, providers []string, brands []string) *sqlchemy.SSubQuery {
	return manager.GetProviderFieldQuery("id", isPublic, isOnPremise, providers, brands)
}

func (manager *SCloudproviderManager) GetPublicProviderProvidersQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderProvidersQuery(tristate.True, tristate.None)
}

func (manager *SCloudproviderManager) GetPrivateProviderProvidersQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderProvidersQuery(tristate.False, tristate.False)
}

func (manager *SCloudproviderManager) GetOnPremiseProviderProvidersQuery() *sqlchemy.SSubQuery {
	return manager.GetProviderProvidersQuery(tristate.None, tristate.True)
}

func (manager *SCloudproviderManager) GetProviderProvidersQuery(isPublic tristate.TriState, isOnPremise tristate.TriState) *sqlchemy.SSubQuery {
	return manager.GetProviderFieldQuery("provider", isPublic, isOnPremise, nil, nil)
}

func (manager *SCloudproviderManager) GetProviderFieldQuery(field string, isPublic tristate.TriState, isOnPremise tristate.TriState, providers []string, brands []string) *sqlchemy.SSubQuery {
	q := manager.Query(field).Distinct()
	account := CloudaccountManager.Query().SubQuery()
	q = q.Join(account, sqlchemy.Equals(
		account.Field("id"), q.Field("cloudaccount_id")),
	)
	if isPublic.IsTrue() {
		q = q.Filter(sqlchemy.IsTrue(account.Field("is_public_cloud")))
	} else if isPublic.IsFalse() {
		q = q.Filter(sqlchemy.IsFalse(account.Field("is_public_cloud")))
	}
	if isOnPremise.IsTrue() {
		q = q.Filter(sqlchemy.IsTrue(account.Field("is_on_premise")))
	} else if isOnPremise.IsFalse() {
		q = q.Filter(sqlchemy.IsFalse(account.Field("is_on_premise")))
	}
	if len(providers) > 0 || len(brands) > 0 {
		q = q.Filter(sqlchemy.OR(
			sqlchemy.In(account.Field("provider"), providers),
			sqlchemy.In(account.Field("brand"), brands),
		))
	}
	return q.SubQuery()
}

func CloudProviderFilter(q *sqlchemy.SQuery, managerIdField sqlchemy.IQueryField, providers []string, brands []string, cloudEnv string) *sqlchemy.SQuery {
	if len(cloudEnv) == 0 && len(providers) == 0 && len(brands) == 0 {
		return q
	}
	isPublic := tristate.None
	isOnPremise := tristate.None
	includeOneCloud := false
	switch cloudEnv {
	case api.CLOUD_ENV_PUBLIC_CLOUD:
		isPublic = tristate.True
	case api.CLOUD_ENV_PRIVATE_CLOUD:
		isPublic = tristate.False
		isOnPremise = tristate.False
	case api.CLOUD_ENV_ON_PREMISE:
		isOnPremise = tristate.True
		includeOneCloud = true
	default:
		includeOneCloud = true
	}
	if includeOneCloud && len(providers) > 0 && !utils.IsInStringArray(api.CLOUD_PROVIDER_ONECLOUD, providers) {
		includeOneCloud = false
	}
	if includeOneCloud && len(brands) > 0 && !utils.IsInStringArray(api.CLOUD_PROVIDER_ONECLOUD, brands) {
		includeOneCloud = false
	}
	subq := CloudproviderManager.GetProviderIdsQuery(isPublic, isOnPremise, providers, brands)
	if includeOneCloud {
		return q.Filter(sqlchemy.OR(
			sqlchemy.In(managerIdField, subq),
			sqlchemy.IsNullOrEmpty(managerIdField),
		))
	} else {
		return q.Filter(sqlchemy.In(managerIdField, subq))
	}
}

func (self *SCloudprovider) CleanSchedCache() {
	hosts := []SHost{}
	q := HostManager.Query().Equals("manager_id", self.Id)
	if err := db.FetchModelObjects(HostManager, q, &hosts); err != nil {
		log.Errorf("failed to get hosts for cloudprovider %s error: %v", self.Name, err)
		return
	}
	for _, host := range hosts {
		host.ClearSchedDescCache()
	}
}

func (self *SCloudprovider) GetGuestCount() (int, error) {
	sq := HostManager.Query("id").Equals("manager_id", self.Id)
	return GuestManager.Query().In("host_id", sq).CountWithError()
}

func (self *SCloudprovider) GetHostCount() (int, error) {
	return HostManager.Query().Equals("manager_id", self.Id).IsFalse("is_emulated").CountWithError()
}

func (self *SCloudprovider) getVpcCount() (int, error) {
	return VpcManager.Query().Equals("manager_id", self.Id).IsFalse("is_emulated").CountWithError()
}

func (self *SCloudprovider) getStorageCount() (int, error) {
	return StorageManager.Query().Equals("manager_id", self.Id).IsFalse("is_emulated").CountWithError()
}

func (self *SCloudprovider) getStoragecacheCount() (int, error) {
	return StoragecacheManager.Query().Equals("manager_id", self.Id).CountWithError()
}

func (self *SCloudprovider) getEipCount() (int, error) {
	return ElasticipManager.Query().Equals("manager_id", self.Id).CountWithError()
}

func (self *SCloudprovider) getSnapshotCount() (int, error) {
	return SnapshotManager.Query().Equals("manager_id", self.Id).CountWithError()
}

func (self *SCloudprovider) getLoadbalancerCount() (int, error) {
	return LoadbalancerManager.Query().Equals("manager_id", self.Id).CountWithError()
}

func (self *SCloudprovider) getDBInstanceCount() (int, error) {
	q := DBInstanceManager.Query()
	q = q.Filter(sqlchemy.Equals(q.Field("manager_id"), self.Id))
	return q.CountWithError()
}

func (self *SCloudprovider) getElasticcacheCount() (int, error) {
	vpcs := VpcManager.Query("id", "manager_id").SubQuery()
	q := ElasticcacheManager.Query()
	q = q.Join(vpcs, sqlchemy.Equals(q.Field("vpc_id"), vpcs.Field("id")))
	q = q.Filter(sqlchemy.Equals(vpcs.Field("manager_id"), self.Id))
	return q.CountWithError()
}

func (self *SCloudprovider) getExternalProjectCount() (int, error) {
	return ExternalProjectManager.Query().Equals("manager_id", self.Id).CountWithError()
}

func (self *SCloudprovider) getSyncRegionCount() (int, error) {
	return CloudproviderRegionManager.Query().Equals("cloudprovider_id", self.Id).CountWithError()
}

func (self *SCloudprovider) ValidateUpdateData(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input api.CloudproviderUpdateInput) (api.CloudproviderUpdateInput, error) {
	var err error
	input.EnabledStatusStandaloneResourceBaseUpdateInput, err = self.SEnabledStatusStandaloneResourceBase.ValidateUpdateData(ctx, userCred, query, input.EnabledStatusStandaloneResourceBaseUpdateInput)
	if err != nil {
		return input, errors.Wrap(err, "SEnabledStatusStandaloneResourceBase.ValidateUpdateData")
	}
	return input, nil
}

// +onecloud:swagger-gen-ignore
func (self *SCloudproviderManager) ValidateCreateData(ctx context.Context, userCred mcclient.TokenCredential, ownerId mcclient.IIdentityProvider, query jsonutils.JSONObject, input api.CloudproviderCreateInput) (api.CloudproviderCreateInput, error) {
	return input, httperrors.NewUnsupportOperationError("Directly creating cloudprovider is not supported, create cloudaccount instead")
}

func (self *SCloudprovider) getAccessUrl() string {
	if len(self.AccessUrl) > 0 {
		return self.AccessUrl
	}
	account, _ := self.GetCloudaccount()
	if account != nil {
		return account.AccessUrl
	}
	return ""
}

func (self *SCloudprovider) getPassword() (string, error) {
	if len(self.Secret) == 0 {
		account, err := self.GetCloudaccount()
		if err != nil {
			return "", errors.Wrapf(err, "GetCloudaccount")
		}
		return account.getPassword()
	}
	return utils.DescryptAESBase64(self.Id, self.Secret)
}

func getTenant(ctx context.Context, projectId string, name string) (*db.STenant, error) {
	if len(projectId) > 0 {
		tenant, err := db.TenantCacheManager.FetchTenantById(ctx, projectId)
		if err != nil {
			return nil, errors.Wrap(err, "TenantCacheManager.FetchTenantById")
		}
		return tenant, nil
	}
	if len(name) == 0 {
		return nil, errors.Error("cannot syncProject for empty name")
	}
	return db.TenantCacheManager.FetchTenantByName(ctx, name)
}

func createTenant(ctx context.Context, name, domainId, desc string) (string, string, error) {
	s := auth.GetAdminSession(ctx, options.Options.Region)
	params := jsonutils.NewDict()
	params.Add(jsonutils.NewString(name), "generate_name")

	params.Add(jsonutils.NewString(domainId), "domain_id")
	params.Add(jsonutils.NewString(desc), "description")

	resp, err := identity.Projects.Create(s, params)
	if err != nil {
		return "", "", errors.Wrap(err, "Projects.Create")
	}
	projectId, err := resp.GetString("id")
	if err != nil {
		return "", "", errors.Wrap(err, "resp.GetString")
	}
	return domainId, projectId, nil
}

func (self *SCloudaccount) getOrCreateTenant(ctx context.Context, name, domainId, projectId, desc string) (string, string, error) {
	if len(domainId) == 0 {
		domainId = self.DomainId
	}
	tenant, err := getTenant(ctx, projectId, name)
	if err != nil {
		if errors.Cause(err) != sql.ErrNoRows {
			return "", "", errors.Wrapf(err, "getTenan")
		}
		return createTenant(ctx, name, domainId, desc)
	}
	share := self.GetSharedInfo()
	if tenant.DomainId == self.DomainId || (share.PublicScope == rbacscope.ScopeSystem ||
		(share.PublicScope == rbacscope.ScopeDomain && utils.IsInStringArray(tenant.DomainId, share.SharedDomains))) {
		return tenant.DomainId, tenant.Id, nil
	}
	return createTenant(ctx, name, domainId, desc)
}

func (self *SCloudprovider) syncProject(ctx context.Context, userCred mcclient.TokenCredential) error {
	account, err := self.GetCloudaccount()
	if err != nil {
		return errors.Wrapf(err, "GetCloudaccount")
	}

	desc := fmt.Sprintf("auto create from cloud provider %s (%s)", self.Name, self.Id)
	domainId, projectId, err := account.getOrCreateTenant(ctx, self.Name, "", self.ProjectId, desc)
	if err != nil {
		return errors.Wrap(err, "getOrCreateTenant")
	}

	return self.saveProject(userCred, domainId, projectId)
}

func (self *SCloudprovider) saveProject(userCred mcclient.TokenCredential, domainId, projectId string) error {
	if projectId != self.ProjectId {
		diff, err := db.Update(self, func() error {
			self.DomainId = domainId
			self.ProjectId = projectId
			return nil
		})
		if err != nil {
			log.Errorf("update projectId fail: %s", err)
			return err
		}
		db.OpsLog.LogEvent(self, db.ACT_UPDATE, diff, userCred)
	}
	return nil
}

type SSyncRange struct {
	api.SyncRangeInput
}

func (sr *SSyncRange) GetRegionIds() ([]string, error) {
	regionIds := []string{}
	if len(sr.Host) == 0 && len(sr.Zone) == 0 && len(sr.Region) == 0 {
		return regionIds, nil
	}
	hostQ := HostManager.Query().SubQuery()
	hosts := hostQ.Query().Filter(sqlchemy.OR(
		sqlchemy.In(hostQ.Field("id"), sr.Host),
		sqlchemy.In(hostQ.Field("name"), sr.Host),
	)).SubQuery()
	zoneQ := ZoneManager.Query().SubQuery()
	zones := zoneQ.Query().Filter(sqlchemy.OR(
		sqlchemy.In(zoneQ.Field("id"), sr.Zone),
		sqlchemy.In(zoneQ.Field("name"), sr.Zone),
		sqlchemy.In(zoneQ.Field("id"), hosts.Query(hosts.Field("zone_id")).SubQuery()),
	)).SubQuery()
	regionQ := CloudregionManager.Query().SubQuery()
	q := regionQ.Query(regionQ.Field("id")).Filter(sqlchemy.OR(
		sqlchemy.In(regionQ.Field("id"), sr.Region),
		sqlchemy.In(regionQ.Field("name"), sr.Region),
		sqlchemy.In(regionQ.Field("id"), zones.Query(zones.Field("cloudregion_id")).SubQuery()),
	))
	rows, err := q.Rows()
	if err != nil {
		return nil, errors.Wrap(err, "q.Rows")
	}
	defer rows.Close()
	for rows.Next() {
		var regionId string
		err = rows.Scan(&regionId)
		if err != nil {
			return nil, errors.Wrap(err, "rows.Scan")
		}
		regionIds = append(regionIds, regionId)
	}
	return regionIds, nil
}

func (sr *SSyncRange) NeedSyncResource(res string) bool {
	if sr.FullSync {
		return true
	}

	if len(sr.Resources) == 0 {
		return true
	}
	return utils.IsInStringArray(res, sr.Resources)
}

func (sr *SSyncRange) NeedSyncInfo() bool {
	if sr.FullSync {
		return true
	}
	if len(sr.Region) > 0 || len(sr.Zone) > 0 || len(sr.Host) > 0 || len(sr.Resources) > 0 {
		return true
	}
	return false
}

func (sr *SSyncRange) normalizeRegionIds() error {
	for i := 0; i < len(sr.Region); i += 1 {
		obj, err := CloudregionManager.FetchByIdOrName(nil, sr.Region[i])
		if err != nil {
			if err == sql.ErrNoRows {
				return httperrors.NewResourceNotFoundError("Region %s not found", sr.Region[i])
			} else {
				return err
			}
		}
		sr.Region[i] = obj.GetId()
	}
	return nil
}

func (sr *SSyncRange) normalizeZoneIds() error {
	for i := 0; i < len(sr.Zone); i += 1 {
		obj, err := ZoneManager.FetchByIdOrName(nil, sr.Zone[i])
		if err != nil {
			if err == sql.ErrNoRows {
				return httperrors.NewResourceNotFoundError("Zone %s not found", sr.Zone[i])
			} else {
				return err
			}
		}
		zone := obj.(*SZone)
		region, _ := zone.GetRegion()
		if region == nil {
			continue
		}
		sr.Zone[i] = zone.GetId()
		if !utils.IsInStringArray(region.Id, sr.Region) {
			sr.Region = append(sr.Region, region.Id)
		}
	}
	return nil
}

func (sr *SSyncRange) normalizeHostIds() error {
	for i := 0; i < len(sr.Host); i += 1 {
		obj, err := HostManager.FetchByIdOrName(nil, sr.Host[i])
		if err != nil {
			if err == sql.ErrNoRows {
				return httperrors.NewResourceNotFoundError("Host %s not found", sr.Host[i])
			} else {
				return err
			}
		}
		host := obj.(*SHost)
		zone, _ := host.GetZone()
		if zone == nil {
			continue
		}
		region, _ := zone.GetRegion()
		if region == nil {
			continue
		}
		sr.Host[i] = host.GetId()
		if !utils.IsInStringArray(zone.Id, sr.Zone) {
			sr.Zone = append(sr.Zone, zone.Id)
		}
		if !utils.IsInStringArray(region.Id, sr.Region) {
			sr.Region = append(sr.Region, region.Id)
		}
	}
	return nil
}

func (sr *SSyncRange) Normalize() error {
	if sr.Region != nil && len(sr.Region) > 0 {
		err := sr.normalizeRegionIds()
		if err != nil {
			return err
		}
	} else {
		sr.Region = make([]string, 0)
	}
	if sr.Zone != nil && len(sr.Zone) > 0 {
		err := sr.normalizeZoneIds()
		if err != nil {
			return err
		}
	} else {
		sr.Zone = make([]string, 0)
	}
	if sr.Host != nil && len(sr.Host) > 0 {
		err := sr.normalizeHostIds()
		if err != nil {
			return err
		}
	} else {
		sr.Host = make([]string, 0)
	}
	return nil
}

func (self *SCloudprovider) PerformSync(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input api.SyncRangeInput) (jsonutils.JSONObject, error) {
	if !self.GetEnabled() {
		return nil, httperrors.NewInvalidStatusError("Cloudprovider disabled")
	}
	account, err := self.GetCloudaccount()
	if err != nil {
		return nil, errors.Wrapf(err, "GetCloudaccount")
	}
	if !account.GetEnabled() {
		return nil, httperrors.NewInvalidStatusError("Cloudaccount disabled")
	}
	syncRange := SSyncRange{input}
	if syncRange.FullSync || len(syncRange.Region) > 0 || len(syncRange.Zone) > 0 || len(syncRange.Host) > 0 || len(syncRange.Resources) > 0 {
		syncRange.DeepSync = true
	}
	if self.CanSync() || syncRange.Force {
		return nil, self.StartSyncCloudProviderInfoTask(ctx, userCred, &syncRange, "")
	}
	return nil, httperrors.NewInvalidStatusError("Unable to synchronize frequently")
}

func (self *SCloudprovider) StartSyncCloudProviderInfoTask(ctx context.Context, userCred mcclient.TokenCredential, syncRange *SSyncRange, parentTaskId string) error {
	params := jsonutils.NewDict()
	if syncRange != nil {
		params.Add(jsonutils.Marshal(syncRange), "sync_range")
	}
	task, err := taskman.TaskManager.NewTask(ctx, "CloudProviderSyncInfoTask", self, userCred, params, parentTaskId, "", nil)
	if err != nil {
		return errors.Wrapf(err, "NewTask")
	}
	if cloudaccount, _ := self.GetCloudaccount(); cloudaccount != nil {
		cloudaccount.MarkSyncing(userCred, false)
	}
	self.markStartSync(userCred, syncRange)
	db.OpsLog.LogEvent(self, db.ACT_SYNC_HOST_START, "", userCred)
	return task.ScheduleRun(nil)
}

func (self *SCloudprovider) PerformChangeProject(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input apis.PerformChangeProjectOwnerInput) (jsonutils.JSONObject, error) {
	project := input.ProjectId

	tenant, err := db.TenantCacheManager.FetchTenantByIdOrName(ctx, project)
	if err != nil {
		return nil, httperrors.NewNotFoundError("project %s not found", project)
	}

	if self.ProjectId == tenant.Id {
		return nil, nil
	}

	account, err := self.GetCloudaccount()
	if err != nil {
		return nil, err
	}
	if self.DomainId != tenant.DomainId {
		if !db.IsAdminAllowPerform(ctx, userCred, self, "change-project") {
			return nil, httperrors.NewForbiddenError("not allow to change project across domain")
		}
		if account.ShareMode == api.CLOUD_ACCOUNT_SHARE_MODE_ACCOUNT_DOMAIN && account.DomainId != tenant.DomainId {
			return nil, httperrors.NewInvalidStatusError("cannot change to a different domain from a private cloud account")
		}
		// if account's public_scope=domain and share_mode=provider_domain, only allow to share to specific domains
		if account.PublicScope == string(rbacscope.ScopeDomain) {
			sharedDomains := account.GetSharedDomains()
			if !utils.IsInStringArray(tenant.DomainId, sharedDomains) && account.DomainId != tenant.DomainId {
				return nil, errors.Wrap(httperrors.ErrForbidden, "cannot set to domain outside of the shared domains")
			}
		}
		// otherwise, allow change project across domain
	}

	notes := struct {
		OldProjectId string
		OldDomainId  string
		NewProjectId string
		NewProject   string
		NewDomainId  string
		NewDomain    string
	}{
		OldProjectId: self.ProjectId,
		OldDomainId:  self.DomainId,
		NewProjectId: tenant.Id,
		NewProject:   tenant.Name,
		NewDomainId:  tenant.DomainId,
		NewDomain:    tenant.Domain,
	}

	err = self.saveProject(userCred, tenant.DomainId, tenant.Id)
	if err != nil {
		log.Errorf("Update cloudprovider error: %v", err)
		return nil, httperrors.NewGeneralError(err)
	}

	logclient.AddSimpleActionLog(self, logclient.ACT_CHANGE_OWNER, notes, userCred, true)

	return nil, self.StartSyncCloudProviderInfoTask(ctx, userCred, &SSyncRange{SyncRangeInput: api.SyncRangeInput{
		FullSync: true, DeepSync: true,
	}}, "")
}

func (self *SCloudprovider) markStartingSync(userCred mcclient.TokenCredential, syncRange *SSyncRange) error {
	_, err := db.Update(self, func() error {
		self.SyncStatus = api.CLOUD_PROVIDER_SYNC_STATUS_QUEUING
		return nil
	})
	if err != nil {
		return errors.Wrap(err, "db.Update")
	}
	cprs := self.GetCloudproviderRegions()
	for i := range cprs {
		if cprs[i].Enabled {
			err := cprs[i].markStartingSync(userCred, syncRange)
			if err != nil {
				return errors.Wrap(err, "cprs[i].markStartingSync")
			}
		}
	}
	return nil
}

func (self *SCloudprovider) markStartSync(userCred mcclient.TokenCredential, syncRange *SSyncRange) error {
	_, err := db.Update(self, func() error {
		self.SyncStatus = api.CLOUD_PROVIDER_SYNC_STATUS_QUEUED
		return nil
	})
	if err != nil {
		return errors.Wrapf(err, "db.Update")
	}
	cprs := self.GetCloudproviderRegions()
	for i := range cprs {
		if cprs[i].Enabled {
			err := cprs[i].markStartingSync(userCred, syncRange)
			if err != nil {
				return errors.Wrap(err, "cprs[i].markStartingSync")
			}
		}
	}
	return nil
}

func (self *SCloudprovider) markSyncing(userCred mcclient.TokenCredential) error {
	_, err := db.Update(self, func() error {
		self.SyncStatus = api.CLOUD_PROVIDER_SYNC_STATUS_SYNCING
		self.LastSync = timeutils.UtcNow()
		self.LastSyncEndAt = time.Time{}
		return nil
	})
	if err != nil {
		log.Errorf("Failed to markSyncing error: %v", err)
		return err
	}
	return nil
}

func (self *SCloudprovider) markEndSyncWithLock(ctx context.Context, userCred mcclient.TokenCredential) error {
	err := func() error {
		lockman.LockObject(ctx, self)
		defer lockman.ReleaseObject(ctx, self)

		if self.SyncStatus == api.CLOUD_PROVIDER_SYNC_STATUS_IDLE {
			return nil
		}

		if self.getSyncStatus2() != api.CLOUD_PROVIDER_SYNC_STATUS_IDLE {
			return nil
		}

		err := self.markEndSync(userCred)
		if err != nil {
			return err
		}
		return nil
	}()

	if err != nil {
		return err
	}

	account, err := self.GetCloudaccount()
	if err != nil {
		return errors.Wrapf(err, "GetCloudaccount")
	}
	return account.MarkEndSyncWithLock(ctx, userCred)
}

func (self *SCloudprovider) markEndSync(userCred mcclient.TokenCredential) error {
	_, err := db.Update(self, func() error {
		self.SyncStatus = api.CLOUD_PROVIDER_SYNC_STATUS_IDLE
		self.LastSyncEndAt = timeutils.UtcNow()
		return nil
	})
	if err != nil {
		return errors.Wrapf(err, "markEndSync")
	}
	return nil
}

func (self *SCloudprovider) cancelStartingSync(userCred mcclient.TokenCredential) error {
	if self.SyncStatus == api.CLOUD_PROVIDER_SYNC_STATUS_QUEUING {
		cprs := self.GetCloudproviderRegions()
		for i := range cprs {
			err := cprs[i].cancelStartingSync(userCred)
			if err != nil {
				return errors.Wrap(err, "cprs[i].cancelStartingSync")
			}
		}
		_, err := db.Update(self, func() error {
			self.SyncStatus = api.CLOUD_PROVIDER_SYNC_STATUS_IDLE
			return nil
		})
		if err != nil {
			return errors.Wrap(err, "db.Update")
		}
	}
	return nil
}

func (self *SCloudprovider) GetProviderFactory() (cloudprovider.ICloudProviderFactory, error) {
	return cloudprovider.GetProviderFactory(self.Provider)
}

func (self *SCloudprovider) GetProvider(ctx context.Context) (cloudprovider.ICloudProvider, error) {
	if !self.GetEnabled() {
		return nil, errors.Wrap(httperrors.ErrInvalidStatus, "Cloud provider is not enabled")
	}

	accessUrl := self.getAccessUrl()
	passwd, err := self.getPassword()
	if err != nil {
		return nil, err
	}

	account, err := self.GetCloudaccount()
	if err != nil {
		return nil, errors.Wrapf(err, "GetCloudaccount")
	}
	defaultRegion, _ := jsonutils.Marshal(account.Options).GetString("default_region")
	return cloudprovider.GetProvider(cloudprovider.ProviderConfig{
		Id:        self.Id,
		Name:      self.Name,
		Vendor:    self.Provider,
		URL:       accessUrl,
		Account:   self.Account,
		Secret:    passwd,
		ProxyFunc: account.proxyFunc(),

		AliyunResourceGroupIds: options.Options.AliyunResourceGroups,

		ReadOnly: account.ReadOnly,

		DefaultRegion: defaultRegion,
		Options:       account.Options,

		UpdatePermission: account.UpdatePermission(ctx),
	})
}

func (self *SCloudprovider) savePassword(secret string) error {
	sec, err := utils.EncryptAESBase64(self.Id, secret)
	if err != nil {
		return err
	}

	_, err = db.Update(self, func() error {
		self.Secret = sec
		return nil
	})
	return err
}

func (self *SCloudprovider) GetCloudaccount() (*SCloudaccount, error) {
	obj, err := CloudaccountManager.FetchById(self.CloudaccountId)
	if err != nil {
		return nil, errors.Wrapf(err, "FetchById(%s)", self.CloudaccountId)
	}
	return obj.(*SCloudaccount), nil
}

func (manager *SCloudproviderManager) FetchCloudproviderById(providerId string) *SCloudprovider {
	providerObj, err := manager.FetchById(providerId)
	if err != nil {
		return nil
	}
	return providerObj.(*SCloudprovider)
}

func IsProviderAccountEnabled(providerId string) bool {
	if len(providerId) == 0 {
		return true
	}
	return CloudproviderManager.IsProviderAccountEnabled(providerId)
}

func (manager *SCloudproviderManager) IsProviderAccountEnabled(providerId string) bool {
	providerObj := manager.FetchCloudproviderById(providerId)
	if providerObj == nil {
		return false
	}
	if !providerObj.GetEnabled() {
		return false
	}
	account, _ := providerObj.GetCloudaccount()
	if account == nil {
		return false
	}
	return account.GetEnabled()
}

func (manager *SCloudproviderManager) FetchCloudproviderByIdOrName(providerId string) *SCloudprovider {
	providerObj, err := manager.FetchByIdOrName(nil, providerId)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Errorf("%s", err)
		}
		return nil
	}
	return providerObj.(*SCloudprovider)
}

func (self *SCloudprovider) getUsage() api.SCloudproviderUsage {
	usage := api.SCloudproviderUsage{}

	usage.GuestCount, _ = self.GetGuestCount()
	usage.HostCount, _ = self.GetHostCount()
	usage.VpcCount, _ = self.getVpcCount()
	usage.StorageCount, _ = self.getStorageCount()
	usage.StorageCacheCount, _ = self.getStoragecacheCount()
	usage.EipCount, _ = self.getEipCount()
	usage.SnapshotCount, _ = self.getSnapshotCount()
	usage.LoadbalancerCount, _ = self.getLoadbalancerCount()
	usage.DBInstanceCount, _ = self.getDBInstanceCount()
	usage.ElasticcacheCount, _ = self.getElasticcacheCount()
	usage.ProjectCount, _ = self.getExternalProjectCount()
	usage.SyncRegionCount, _ = self.getSyncRegionCount()

	return usage
}

func (self *SCloudprovider) getProject(ctx context.Context) *db.STenant {
	proj, _ := db.TenantCacheManager.FetchTenantById(ctx, self.ProjectId)
	return proj
}

func (manager *SCloudproviderManager) FetchCustomizeColumns(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	query jsonutils.JSONObject,
	objs []interface{},
	fields stringutils2.SSortedStrings,
	isList bool,
) []api.CloudproviderDetails {
	rows := make([]api.CloudproviderDetails, len(objs))

	stdRows := manager.SEnabledStatusStandaloneResourceBaseManager.FetchCustomizeColumns(ctx, userCred, query, objs, fields, isList)
	projRows := manager.SProjectizedResourceBaseManager.FetchCustomizeColumns(ctx, userCred, query, objs, fields, isList)
	pmRows := manager.SProjectMappingResourceBaseManager.FetchCustomizeColumns(ctx, userCred, query, objs, fields, isList)
	accountIds := make([]string, len(objs))
	for i := range rows {
		provider := objs[i].(*SCloudprovider)
		accountIds[i] = provider.CloudaccountId
		rows[i] = api.CloudproviderDetails{
			EnabledStatusStandaloneResourceDetails: stdRows[i],
			ProjectizedResourceInfo:                projRows[i],
			SCloudproviderUsage:                    provider.getUsage(),
			SyncStatus2:                            provider.getSyncStatus2(),
			ProjectMappingResourceInfo:             pmRows[i],
		}
		capabilities, _ := CloudproviderCapabilityManager.getCapabilities(provider.Id)
		if len(capabilities) > 0 {
			rows[i].Capabilities = capabilities
		}
	}

	accounts := make(map[string]SCloudaccount)
	err := db.FetchStandaloneObjectsByIds(CloudaccountManager, accountIds, &accounts)
	if err != nil {
		log.Errorf("FetchStandaloneObjectsByIds (%s) fail %s",
			CloudaccountManager.KeywordPlural(), err)
		return rows
	}

	proxySettingIds := make([]string, len(accounts))
	for i := range accounts {
		proxySettingId := accounts[i].ProxySettingId
		if !utils.IsInStringArray(proxySettingId, proxySettingIds) {
			proxySettingIds = append(proxySettingIds, proxySettingId)
		}
	}
	proxySettings := make(map[string]proxy.SProxySetting)
	err = db.FetchStandaloneObjectsByIds(proxy.ProxySettingManager, proxySettingIds, &proxySettings)
	if err != nil {
		log.Errorf("FetchStandaloneObjectsByIds (%s) fail %s",
			proxy.ProxySettingManager.KeywordPlural(), err)
		return rows
	}

	for i := range rows {
		if account, ok := accounts[accountIds[i]]; ok {
			rows[i].Cloudaccount = account.Name
			rows[i].ReadOnly = account.ReadOnly
			rows[i].Brand = account.Brand

			ps := &rows[i].ProxySetting
			if proxySetting, ok := proxySettings[account.ProxySettingId]; ok {
				ps.Id = proxySetting.Id
				ps.Name = proxySetting.Name
				ps.HTTPProxy = proxySetting.HTTPProxy
				ps.HTTPSProxy = proxySetting.HTTPSProxy
				ps.NoProxy = proxySetting.NoProxy
			}
		}
	}

	return rows
}

func (manager *SCloudproviderManager) InitializeData() error {
	// fill empty projectId with system project ID
	providers := make([]SCloudprovider, 0)
	q := CloudproviderManager.Query()
	q = q.Filter(sqlchemy.OR(sqlchemy.IsEmpty(q.Field("tenant_id")), sqlchemy.IsNull(q.Field("tenant_id"))))
	err := db.FetchModelObjects(CloudproviderManager, q, &providers)
	if err != nil {
		log.Errorf("query cloudproviders with empty tenant_id fail %s", err)
		return err
	}
	for i := 0; i < len(providers); i += 1 {
		_, err := db.Update(&providers[i], func() error {
			providers[i].DomainId = auth.AdminCredential().GetProjectDomainId()
			providers[i].ProjectId = auth.AdminCredential().GetProjectId()
			return nil
		})
		if err != nil {
			log.Errorf("update cloudprovider project fail %s", err)
			return err
		}
	}

	return nil
}

// 云订阅列表
func (manager *SCloudproviderManager) ListItemFilter(
	ctx context.Context,
	q *sqlchemy.SQuery,
	userCred mcclient.TokenCredential,
	query api.CloudproviderListInput,
) (*sqlchemy.SQuery, error) {
	accountArr := query.CloudaccountId
	if len(accountArr) > 0 {
		cpq := CloudaccountManager.Query().SubQuery()
		subcpq := cpq.Query(cpq.Field("id")).Filter(sqlchemy.OR(
			sqlchemy.In(cpq.Field("id"), stringutils2.RemoveUtf8Strings(accountArr)),
			sqlchemy.In(cpq.Field("name"), accountArr),
		)).SubQuery()
		q = q.In("cloudaccount_id", subcpq)
	}

	var zone *SZone
	var region *SCloudregion

	if len(query.ZoneId) > 0 {
		zoneObj, err := ZoneManager.FetchByIdOrName(userCred, query.ZoneId)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, errors.Wrapf(httperrors.ErrResourceNotFound, "%s %s", ZoneManager.Keyword(), query.ZoneId)
			} else {
				return nil, errors.Wrap(err, "ZoneManager.FetchByIdOrName")
			}
		}
		zone = zoneObj.(*SZone)
		pr := CloudproviderRegionManager.Query().SubQuery()
		sq := pr.Query(pr.Field("cloudprovider_id")).Equals("cloudregion_id", zone.CloudregionId).Distinct()
		q = q.In("id", sq)
	} else if len(query.CloudregionId) > 0 {
		regionObj, err := CloudregionManager.FetchByIdOrName(userCred, query.CloudregionId)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, httperrors.NewResourceNotFoundError2("cloudregion", query.CloudregionId)
			}
			return nil, httperrors.NewGeneralError(err)
		}
		region = regionObj.(*SCloudregion)
		pr := CloudproviderRegionManager.Query().SubQuery()
		sq := pr.Query(pr.Field("cloudprovider_id")).Equals("cloudregion_id", region.Id).Distinct()
		q = q.In("id", sq)
	}

	if query.Usable != nil && *query.Usable {
		providers := usableCloudProviders().SubQuery()
		networks := NetworkManager.Query().SubQuery()
		wires := WireManager.Query().SubQuery()
		vpcs := VpcManager.Query().SubQuery()
		providerRegions := CloudproviderRegionManager.Query().SubQuery()

		sq := providers.Query(sqlchemy.DISTINCT("id", providers.Field("id")))
		sq = sq.Join(providerRegions, sqlchemy.Equals(providers.Field("id"), providerRegions.Field("cloudprovider_id")))
		sq = sq.Join(vpcs, sqlchemy.Equals(providerRegions.Field("cloudregion_id"), vpcs.Field("cloudregion_id")))
		sq = sq.Join(wires, sqlchemy.Equals(vpcs.Field("id"), wires.Field("vpc_id")))
		sq = sq.Join(networks, sqlchemy.Equals(wires.Field("id"), networks.Field("wire_id")))
		sq = sq.Filter(sqlchemy.Equals(vpcs.Field("status"), api.VPC_STATUS_AVAILABLE))
		sq = sq.Filter(sqlchemy.Equals(networks.Field("status"), api.NETWORK_STATUS_AVAILABLE))
		sq = sq.Filter(sqlchemy.OR(
			sqlchemy.IsNullOrEmpty(vpcs.Field("manager_id")),
			sqlchemy.Equals(vpcs.Field("manager_id"), providers.Field("id")),
		))
		if zone != nil {
			zoneFilter := sqlchemy.OR(sqlchemy.Equals(wires.Field("zone_id"), zone.GetId()), sqlchemy.IsNullOrEmpty(wires.Field("zone_id")))
			sq = sq.Filter(zoneFilter)
		} else if region != nil {
			sq = sq.Filter(sqlchemy.Equals(vpcs.Field("cloudregion_id"), region.GetId()))
		}

		q = q.Filter(sqlchemy.In(q.Field("id"), sq.SubQuery()))
	}

	q, err := manager.SEnabledStatusStandaloneResourceBaseManager.ListItemFilter(ctx, q, userCred, query.EnabledStatusStandaloneResourceListInput)
	if err != nil {
		return nil, errors.Wrap(err, "SEnabledStatusStandaloneResourceBaseManager.ListItemFilter")
	}
	q, err = manager.SProjectizedResourceBaseManager.ListItemFilter(ctx, q, userCred, query.ProjectizedResourceListInput)
	if err != nil {
		return nil, errors.Wrapf(err, "SProjectizedResourceBaseManager.ListItemFilter")
	}
	q, err = manager.SSyncableBaseResourceManager.ListItemFilter(ctx, q, userCred, query.SyncableBaseResourceListInput)
	if err != nil {
		return nil, errors.Wrap(err, "SSyncableBaseResourceManager.ListItemFilter")
	}

	managerStr := query.CloudproviderId
	if len(managerStr) > 0 {
		providerObj, err := manager.FetchByIdOrName(userCred, managerStr)
		if err != nil {
			if err == sql.ErrNoRows {
				return nil, httperrors.NewResourceNotFoundError2(CloudproviderManager.Keyword(), managerStr)
			} else {
				return nil, httperrors.NewGeneralError(err)
			}
		}
		q = q.Equals("id", providerObj.GetId())
	}

	cloudEnvStr := query.CloudEnv
	if cloudEnvStr == api.CLOUD_ENV_PUBLIC_CLOUD {
		cloudaccounts := CloudaccountManager.Query().SubQuery()
		q = q.Join(cloudaccounts, sqlchemy.Equals(cloudaccounts.Field("id"), q.Field("cloudaccount_id")))
		q = q.Filter(sqlchemy.IsTrue(cloudaccounts.Field("is_public_cloud")))
		q = q.Filter(sqlchemy.IsFalse(cloudaccounts.Field("is_on_premise")))
	}

	if cloudEnvStr == api.CLOUD_ENV_PRIVATE_CLOUD {
		cloudaccounts := CloudaccountManager.Query().SubQuery()
		q = q.Join(cloudaccounts, sqlchemy.Equals(cloudaccounts.Field("id"), q.Field("cloudaccount_id")))
		q = q.Filter(sqlchemy.IsFalse(cloudaccounts.Field("is_public_cloud")))
		q = q.Filter(sqlchemy.IsFalse(cloudaccounts.Field("is_on_premise")))
	}

	if cloudEnvStr == api.CLOUD_ENV_ON_PREMISE {
		cloudaccounts := CloudaccountManager.Query().SubQuery()
		q = q.Join(cloudaccounts, sqlchemy.Equals(cloudaccounts.Field("id"), q.Field("cloudaccount_id")))
		q = q.Filter(sqlchemy.IsFalse(cloudaccounts.Field("is_public_cloud")))
		q = q.Filter(sqlchemy.IsTrue(cloudaccounts.Field("is_on_premise")))
	}

	capabilities := query.Capability
	if len(capabilities) > 0 {
		subq := CloudproviderCapabilityManager.Query("cloudprovider_id").In("capability", capabilities).Distinct().SubQuery()
		q = q.In("id", subq)
	}

	if len(query.HealthStatus) > 0 {
		q = q.In("health_status", query.HealthStatus)
	}
	if len(query.Providers) > 0 {
		subq := CloudaccountManager.Query("id").In("provider", query.Providers).SubQuery()
		q = q.In("cloudaccount_id", subq)
	}
	if len(query.Brands) > 0 {
		subq := CloudaccountManager.Query("id").In("brand", query.Brands).SubQuery()
		q = q.In("cloudaccount_id", subq)
	}

	if len(query.HostSchedtagId) > 0 {
		schedTagObj, err := SchedtagManager.FetchByIdOrName(userCred, query.HostSchedtagId)
		if err != nil {
			if errors.Cause(err) == sql.ErrNoRows {
				return nil, errors.Wrapf(httperrors.ErrResourceNotFound, "%s %s", SchedtagManager.Keyword(), query.HostSchedtagId)
			} else {
				return nil, errors.Wrap(err, "SchedtagManager.FetchByIdOrName")
			}
		}
		subq := HostManager.Query("manager_id")
		hostschedtags := HostschedtagManager.Query().Equals("schedtag_id", schedTagObj.GetId()).SubQuery()
		subq = subq.Join(hostschedtags, sqlchemy.Equals(hostschedtags.Field("host_id"), subq.Field("id")))
		log.Debugf("%s", subq.String())
		q = q.In("id", subq.SubQuery())
	}

	return q, nil
}

func (manager *SCloudproviderManager) OrderByExtraFields(
	ctx context.Context,
	q *sqlchemy.SQuery,
	userCred mcclient.TokenCredential,
	query api.CloudproviderListInput,
) (*sqlchemy.SQuery, error) {
	var err error

	q, err = manager.SEnabledStatusStandaloneResourceBaseManager.OrderByExtraFields(ctx, q, userCred, query.EnabledStatusStandaloneResourceListInput)
	if err != nil {
		return nil, errors.Wrap(err, "SEnabledStatusStandaloneResourceBaseManager.OrderByExtraFields")
	}

	return q, nil
}

func (manager *SCloudproviderManager) QueryDistinctExtraField(q *sqlchemy.SQuery, field string) (*sqlchemy.SQuery, error) {
	var err error

	if field == "manager" {
		q = q.AppendField(q.Field("name").Label("manager")).Distinct()
		return q, nil
	}

	if field == "account" {
		accounts := CloudaccountManager.Query("name", "id").SubQuery()
		q.AppendField(accounts.Field("name", field)).Distinct()
		q = q.Join(accounts, sqlchemy.Equals(q.Field("cloudaccount_id"), accounts.Field("id")))
		return q, nil
	}

	q, err = manager.SProjectizedResourceBaseManager.QueryDistinctExtraField(q, field)
	if err == nil {
		return q, nil
	}

	q, err = manager.SEnabledStatusStandaloneResourceBaseManager.QueryDistinctExtraField(q, field)
	if err == nil {
		return q, nil
	}

	return q, httperrors.ErrNotFound
}

func (provider *SCloudprovider) markProviderDisconnected(ctx context.Context, userCred mcclient.TokenCredential, reason string) error {
	_, err := db.UpdateWithLock(ctx, provider, func() error {
		provider.HealthStatus = api.CLOUD_PROVIDER_HEALTH_UNKNOWN
		return nil
	})
	if err != nil {
		return err
	}
	provider.SetStatus(userCred, api.CLOUD_PROVIDER_DISCONNECTED, reason)
	return provider.ClearSchedDescCache()
}

func (self *SCloudprovider) updateName(ctx context.Context, userCred mcclient.TokenCredential, name, desc string) error {
	if self.Name != name || self.Description != desc {
		diff, err := db.Update(self, func() error {
			self.Name = name
			if len(self.Description) == 0 {
				self.Description = desc
			}
			return nil
		})
		if err != nil {
			return errors.Wrapf(err, "db.Update")
		}
		db.OpsLog.LogEvent(self, db.ACT_UPDATE, diff, userCred)
	}
	return nil
}

func (provider *SCloudprovider) markProviderConnected(ctx context.Context, userCred mcclient.TokenCredential, healthStatus string) error {
	if healthStatus != provider.HealthStatus {
		diff, err := db.Update(provider, func() error {
			provider.HealthStatus = healthStatus
			return nil
		})
		if err != nil {
			return err
		}
		db.OpsLog.LogEvent(provider, db.ACT_UPDATE, diff, userCred)
	}
	if provider.Status != api.CLOUD_PROVIDER_CONNECTED {
		provider.SetStatus(userCred, api.CLOUD_PROVIDER_CONNECTED, "")
		return provider.ClearSchedDescCache()
	}
	return nil
}

func (provider *SCloudprovider) prepareCloudproviderRegions(ctx context.Context, userCred mcclient.TokenCredential) ([]SCloudproviderregion, error) {
	driver, err := provider.GetProvider(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "provider.GetProvider")
	}
	err = CloudproviderCapabilityManager.setCapabilities(ctx, userCred, provider.Id, driver.GetCapabilities())
	if err != nil {
		return nil, errors.Wrap(err, "CloudproviderCapabilityManager.setCapabilities")
	}
	if driver.GetFactory().IsOnPremise() {
		cpr := CloudproviderRegionManager.FetchByIdsOrCreate(provider.Id, api.DEFAULT_REGION_ID)
		cpr.setCapabilities(ctx, userCred, driver.GetCapabilities())
		return []SCloudproviderregion{*cpr}, nil
	}
	iregions := driver.GetIRegions()
	externalIdPrefix := driver.GetCloudRegionExternalIdPrefix()
	_, _, cprs, result := CloudregionManager.SyncRegions(ctx, userCred, provider, externalIdPrefix, iregions)
	if result.IsError() {
		log.Errorf("syncRegion fail %s", result.Result())
	}
	return cprs, nil
}

func (provider *SCloudprovider) GetCloudproviderRegions() []SCloudproviderregion {
	q := CloudproviderRegionManager.Query()
	q = q.Equals("cloudprovider_id", provider.Id)
	// q = q.IsTrue("enabled")
	// q = q.Equals("sync_status", api.CLOUD_PROVIDER_SYNC_STATUS_IDLE)

	return CloudproviderRegionManager.fetchRecordsByQuery(q)
}

func (provider *SCloudprovider) resetAutoSync() {
	cprs := provider.GetCloudproviderRegions()
	for i := range cprs {
		cprs[i].resetAutoSync()
	}
}

func (provider *SCloudprovider) syncCloudproviderRegions(ctx context.Context, userCred mcclient.TokenCredential, syncRange SSyncRange, wg *sync.WaitGroup) {
	provider.markSyncing(userCred)
	cprs := provider.GetCloudproviderRegions()
	regionIds, _ := syncRange.GetRegionIds()
	syncCnt := 0
	for i := range cprs {
		if cprs[i].Enabled && cprs[i].CanSync() && (len(regionIds) == 0 || utils.IsInStringArray(cprs[i].CloudregionId, regionIds)) {
			syncCnt += 1
			if wg != nil {
				wg.Add(1)
			}
			cprs[i].submitSyncTask(ctx, userCred, syncRange)
			if wg != nil {
				wg.Done()
			}
		}
	}
	if syncCnt == 0 {
		err := provider.markEndSyncWithLock(ctx, userCred)
		if err != nil {
			log.Errorf("markEndSyncWithLock for %s error: %v", provider.Name, err)
		}
	}
}

func (provider *SCloudprovider) SyncCallSyncCloudproviderRegions(ctx context.Context, userCred mcclient.TokenCredential, syncRange SSyncRange) {
	var wg sync.WaitGroup
	provider.syncCloudproviderRegions(ctx, userCred, syncRange, &wg)
	wg.Wait()
}

func (self *SCloudprovider) IsAvailable() bool {
	if !self.GetEnabled() {
		return false
	}
	if !utils.IsInStringArray(self.Status, api.CLOUD_PROVIDER_VALID_STATUS) {
		return false
	}
	if !utils.IsInStringArray(self.HealthStatus, api.CLOUD_PROVIDER_VALID_HEALTH_STATUS) {
		return false
	}
	return true
}

func (self *SCloudprovider) Delete(ctx context.Context, userCred mcclient.TokenCredential) error {
	// override
	log.Infof("cloud provider delete do nothing")
	return nil
}

func (self *SCloudprovider) RealDelete(ctx context.Context, userCred mcclient.TokenCredential) error {
	var err error

	for _, manager := range []IPurgeableManager{
		BucketManager,
		HostManager,
		SnapshotManager,
		SnapshotPolicyManager,
		StorageManager,
		StoragecacheManager,
		SecurityGroupCacheManager,
		LoadbalancerManager,
		LoadbalancerBackendGroupManager,
		CachedLoadbalancerAclManager,
		CachedLoadbalancerCertificateManager,
		LoadbalancerCertificateManager,
		NatGatewayManager,
		DBInstanceManager,
		DBInstanceBackupManager,
		ElasticcacheManager,
		AccessGroupCacheManager,
		FileSystemManager,
		WafRuleGroupCacheManager,
		WafIPSetCacheManager,
		WafRegexSetCacheManager,
		WafInstanceManager,
		AppManager,
		VpcManager,
		GlobalVpcManager,
		ElasticipManager,
		MongoDBManager,
		ElasticSearchManager,
		KafkaManager,
		CDNDomainManager,
		TablestoreManager,
		NetworkInterfaceManager,
		KubeClusterManager,
		InterVpcNetworkManager,
		CloudproviderRegionManager,
		CloudregionManager,
		CloudproviderQuotaManager,
		ModelartsPoolManager,
	} {
		err = manager.purgeAll(ctx, userCred, self.Id)
		if err != nil {
			return errors.Wrapf(err, "purge %s", manager.Keyword())
		}
		log.Debugf("%s purgeall success!", manager.Keyword())
	}

	CloudproviderCapabilityManager.removeCapabilities(ctx, userCred, self.Id)
	err = DnsZoneCacheManager.removeCaches(ctx, userCred, self.Id)
	if err != nil {
		return errors.Wrapf(err, "remove dns caches")
	}

	return self.SEnabledStatusStandaloneResourceBase.Delete(ctx, userCred)
}

func (self *SCloudprovider) CustomizeDelete(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) error {
	return self.StartCloudproviderDeleteTask(ctx, userCred, "")
}

func (self *SCloudprovider) StartCloudproviderDeleteTask(ctx context.Context, userCred mcclient.TokenCredential, parentTaskId string) error {
	params := jsonutils.NewDict()
	task, err := taskman.TaskManager.NewTask(ctx, "CloudProviderDeleteTask", self, userCred, params, parentTaskId, "", nil)
	if err != nil {
		return errors.Wrapf(err, "NewTask")
	}
	self.SetStatus(userCred, api.CLOUD_PROVIDER_START_DELETE, "StartCloudproviderDeleteTask")
	task.ScheduleRun(nil)
	return nil
}

func (self *SCloudprovider) GetRegionDriver() (IRegionDriver, error) {
	driver := GetRegionDriver(self.Provider)
	if driver == nil {
		return nil, fmt.Errorf("failed to found region driver for %s", self.Provider)
	}
	return driver, nil
}

func (self *SCloudprovider) ClearSchedDescCache() error {
	hosts := make([]SHost, 0)
	q := HostManager.Query().Equals("manager_id", self.Id)
	err := db.FetchModelObjects(HostManager, q, &hosts)
	if err != nil {
		return err
	}
	for i := range hosts {
		err := hosts[i].ClearSchedDescCache()
		if err != nil {
			log.Errorf("host CleanHostSchedCache error: %v", err)
			return err
		}
	}
	return nil
}

func (self *SCloudprovider) PerformEnable(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input apis.PerformEnableInput) (jsonutils.JSONObject, error) {
	if strings.Index(self.Status, "delet") >= 0 {
		return nil, httperrors.NewInvalidStatusError("Cannot enable deleting account")
	}
	_, err := self.SEnabledStatusStandaloneResourceBase.PerformEnable(ctx, userCred, query, input)
	if err != nil {
		return nil, err
	}
	account, err := self.GetCloudaccount()
	if err != nil {
		return nil, err
	}
	if !account.GetEnabled() {
		return account.enableAccountOnly(ctx, userCred, nil, input)
	}
	return nil, nil
}

func (self *SCloudprovider) PerformDisable(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input apis.PerformDisableInput) (jsonutils.JSONObject, error) {
	_, err := self.SEnabledStatusStandaloneResourceBase.PerformDisable(ctx, userCred, query, input)
	if err != nil {
		return nil, err
	}
	account, err := self.GetCloudaccount()
	if err != nil {
		return nil, err
	}
	allDisable := true
	providers := account.GetCloudproviders()
	for i := range providers {
		if providers[i].GetEnabled() {
			allDisable = false
			break
		}
	}
	if allDisable && account.GetEnabled() {
		return account.PerformDisable(ctx, userCred, nil, input)
	}
	return nil, nil
}

func (manager *SCloudproviderManager) filterByDomainId(q *sqlchemy.SQuery, domainId string) *sqlchemy.SQuery {
	subq := db.SharedResourceManager.Query("resource_id")
	subq = subq.Equals("resource_type", CloudaccountManager.Keyword())
	subq = subq.Equals("target_project_id", domainId)
	subq = subq.Equals("target_type", db.SharedTargetDomain)

	cloudaccounts := CloudaccountManager.Query().SubQuery()
	q = q.Join(cloudaccounts, sqlchemy.Equals(
		q.Field("cloudaccount_id"),
		cloudaccounts.Field("id"),
	))
	q = q.Filter(sqlchemy.OR(
		sqlchemy.AND(
			sqlchemy.Equals(q.Field("domain_id"), domainId),
			sqlchemy.Equals(cloudaccounts.Field("share_mode"), api.CLOUD_ACCOUNT_SHARE_MODE_PROVIDER_DOMAIN),
		),
		sqlchemy.AND(
			sqlchemy.Equals(cloudaccounts.Field("share_mode"), api.CLOUD_ACCOUNT_SHARE_MODE_SYSTEM),
			sqlchemy.OR(
				sqlchemy.AND(
					sqlchemy.Equals(cloudaccounts.Field("public_scope"), rbacscope.ScopeNone),
					sqlchemy.Equals(cloudaccounts.Field("domain_id"), domainId),
				),
				sqlchemy.AND(
					sqlchemy.Equals(cloudaccounts.Field("public_scope"), rbacscope.ScopeDomain),
					sqlchemy.OR(
						sqlchemy.Equals(cloudaccounts.Field("domain_id"), domainId),
						sqlchemy.In(cloudaccounts.Field("id"), subq.SubQuery()),
					),
				),
				sqlchemy.Equals(cloudaccounts.Field("public_scope"), rbacscope.ScopeSystem),
			),
		),
		sqlchemy.AND(
			sqlchemy.Equals(cloudaccounts.Field("domain_id"), domainId),
			sqlchemy.Equals(cloudaccounts.Field("share_mode"), api.CLOUD_ACCOUNT_SHARE_MODE_ACCOUNT_DOMAIN),
		),
	))
	return q
}

func (manager *SCloudproviderManager) FilterByOwner(q *sqlchemy.SQuery, owner mcclient.IIdentityProvider, scope rbacscope.TRbacScope) *sqlchemy.SQuery {
	if owner != nil {
		switch scope {
		case rbacscope.ScopeProject, rbacscope.ScopeDomain:
			if len(owner.GetProjectDomainId()) > 0 {
				q = manager.filterByDomainId(q, owner.GetProjectDomainId())
			}
		}
	}
	return q
}

func (self *SCloudprovider) getSyncStatus2() string {
	q := CloudproviderRegionManager.Query()
	q = q.Equals("cloudprovider_id", self.Id)
	q = q.NotEquals("sync_status", api.CLOUD_PROVIDER_SYNC_STATUS_IDLE)

	cnt, err := q.CountWithError()
	if err != nil {
		return api.CLOUD_PROVIDER_SYNC_STATUS_ERROR
	}
	if cnt > 0 {
		return api.CLOUD_PROVIDER_SYNC_STATUS_SYNCING
	} else {
		return api.CLOUD_PROVIDER_SYNC_STATUS_IDLE
	}
}

func (manager *SCloudproviderManager) fetchRecordsByQuery(q *sqlchemy.SQuery) []SCloudprovider {
	recs := make([]SCloudprovider, 0)
	err := db.FetchModelObjects(manager, q, &recs)
	if err != nil {
		return nil
	}
	return recs
}

func (manager *SCloudproviderManager) initAllRecords() {
	recs := manager.fetchRecordsByQuery(manager.Query())
	for i := range recs {
		db.Update(&recs[i], func() error {
			recs[i].SyncStatus = api.CLOUD_PROVIDER_SYNC_STATUS_IDLE
			return nil
		})
	}
}

func (provider *SCloudprovider) GetDetailsClirc(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	accessUrl := provider.getAccessUrl()
	passwd, err := provider.getPassword()
	if err != nil {
		return nil, err
	}

	account, err := provider.GetCloudaccount()
	if err != nil {
		return nil, err
	}

	rc, err := cloudprovider.GetClientRC(provider.Name, accessUrl, provider.Account, passwd, provider.Provider, account.Options)
	if err != nil {
		return nil, err
	}
	return jsonutils.Marshal(rc), nil
}

func (manager *SCloudproviderManager) ResourceScope() rbacscope.TRbacScope {
	return rbacscope.ScopeDomain
}

func (provider *SCloudprovider) GetDetailsStorageClasses(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	input api.CloudproviderGetStorageClassInput,
) (api.CloudproviderGetStorageClassOutput, error) {
	output := api.CloudproviderGetStorageClassOutput{}
	driver, err := provider.GetProvider(ctx)
	if err != nil {
		return output, httperrors.NewInternalServerError("fail to get provider driver %s", err)
	}
	if len(input.CloudregionId) > 0 {
		_, input.CloudregionResourceInput, err = ValidateCloudregionResourceInput(userCred, input.CloudregionResourceInput)
		if err != nil {
			return output, errors.Wrap(err, "ValidateCloudregionResourceInput")
		}
	}

	sc := driver.GetStorageClasses(input.CloudregionId)
	if sc == nil {
		return output, httperrors.NewInternalServerError("storage classes not supported")
	}
	output.StorageClasses = sc
	return output, nil
}

func (provider *SCloudprovider) GetDetailsCannedAcls(
	ctx context.Context,
	userCred mcclient.TokenCredential,
	input api.CloudproviderGetCannedAclInput,
) (api.CloudproviderGetCannedAclOutput, error) {
	output := api.CloudproviderGetCannedAclOutput{}
	driver, err := provider.GetProvider(ctx)
	if err != nil {
		return output, httperrors.NewInternalServerError("fail to get provider driver %s", err)
	}
	if len(input.CloudregionId) > 0 {
		_, input.CloudregionResourceInput, err = ValidateCloudregionResourceInput(userCred, input.CloudregionResourceInput)
		if err != nil {
			return output, errors.Wrap(err, "ValidateCloudregionResourceInput")
		}
	}

	output.BucketCannedAcls = driver.GetBucketCannedAcls(input.CloudregionId)
	output.ObjectCannedAcls = driver.GetObjectCannedAcls(input.CloudregionId)
	return output, nil
}

func (provider *SCloudprovider) getAccountShareInfo() apis.SAccountShareInfo {
	account, _ := provider.GetCloudaccount()
	if account != nil {
		return account.getAccountShareInfo()
	}
	return apis.SAccountShareInfo{}
}

func (provider *SCloudprovider) IsSharable(reqUsrId mcclient.IIdentityProvider) bool {
	account, _ := provider.GetCloudaccount()
	if account != nil {
		if account.ShareMode == api.CLOUD_ACCOUNT_SHARE_MODE_SYSTEM {
			return account.IsSharable(reqUsrId)
		}
	}
	return false
}

func (provider *SCloudprovider) GetDetailsChangeOwnerCandidateDomains(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject) (apis.ChangeOwnerCandidateDomainsOutput, error) {
	return db.IOwnerResourceBaseModelGetChangeOwnerCandidateDomains(provider)
}

func (provider *SCloudprovider) GetChangeOwnerCandidateDomainIds() []string {
	account, _ := provider.GetCloudaccount()
	if account == nil {
		return []string{}
	}
	if account.ShareMode == api.CLOUD_ACCOUNT_SHARE_MODE_ACCOUNT_DOMAIN {
		return []string{account.DomainId}
	}
	// if account's public_scope=domain and share_mode=provider_domain, only allow to share to specific domains
	if account.PublicScope == string(rbacscope.ScopeDomain) {
		sharedDomains := account.GetSharedDomains()
		return append(sharedDomains, account.DomainId)
	}
	return []string{}
}

func (self *SCloudprovider) SyncProject(ctx context.Context, userCred mcclient.TokenCredential, id string) (string, error) {
	if self.Provider == api.CLOUD_PROVIDER_AZURE {
		return self.SyncAzureProject(ctx, userCred, id)
	}
	account, err := self.GetCloudaccount()
	if err != nil {
		return "", errors.Wrapf(err, "GetCloudaccount")
	}
	return account.SyncProject(ctx, userCred, id)
}

func (self *SCloudprovider) GetExternalProjectsByProjectIdOrName(projectId, name string) ([]SExternalProject, error) {
	projects := []SExternalProject{}
	q := ExternalProjectManager.Query().Equals("manager_id", self.Id)
	q = q.Filter(
		sqlchemy.OR(
			sqlchemy.Equals(q.Field("name"), name),
			sqlchemy.Equals(q.Field("tenant_id"), projectId),
		),
	)
	err := db.FetchModelObjects(ExternalProjectManager, q, &projects)
	if err != nil {
		return nil, errors.Wrap(err, "db.FetchModelObjects")
	}
	return projects, nil
}

func (self *SCloudprovider) SyncAzureProject(ctx context.Context, userCred mcclient.TokenCredential, id string) (string, error) {
	lockman.LockRawObject(ctx, "projects", self.Id)
	defer lockman.ReleaseRawObject(ctx, "projects", self.Id)

	account, err := self.GetCloudaccount()
	if err != nil {
		return "", errors.Wrapf(err, "GetCloudaccount")
	}

	provider, err := self.GetProvider(ctx)
	if err != nil {
		return "", errors.Wrap(err, "GetProvider")
	}

	project, err := db.TenantCacheManager.FetchTenantById(ctx, id)
	if err != nil {
		return "", errors.Wrapf(err, "FetchTenantById(%s)", id)
	}

	projects, err := self.GetExternalProjectsByProjectIdOrName(id, project.Name)
	if err != nil {
		return "", errors.Wrapf(err, "GetExternalProjectsByProjectIdOrName(%s,%s)", id, project.Name)
	}

	extProj := GetAvailableExternalProject(project, projects)
	if extProj != nil {
		idx := strings.Index(extProj.ExternalId, "/")
		if idx > -1 {
			return extProj.ExternalId[idx+1:], nil
		}
		return extProj.ExternalId, nil
	}

	retry := 1
	if len(projects) > 0 {
		retry = 10
	}

	var iProject cloudprovider.ICloudProject = nil
	projectName := project.Name
	for i := 0; i < retry; i++ {
		iProject, err = provider.CreateIProject(projectName)
		if err == nil {
			break
		}
		projectName = fmt.Sprintf("%s-%d", project.Name, i)
	}
	if err != nil {
		if errors.Cause(err) != cloudprovider.ErrNotImplemented && errors.Cause(err) != cloudprovider.ErrNotSupported {
			logclient.AddSimpleActionLog(self, logclient.ACT_CREATE, err, userCred, false)
		}
		return "", errors.Wrapf(err, "CreateIProject(%s)", projectName)
	}

	extProj, err = ExternalProjectManager.newFromCloudProject(ctx, userCred, account, project, iProject)
	if err != nil {
		return "", errors.Wrap(err, "newFromCloudProject")
	}

	db.Update(extProj, func() error {
		extProj.ManagerId = self.Id
		return nil
	})

	idx := strings.Index(extProj.ExternalId, "/")
	if idx > -1 {
		return extProj.ExternalId[idx+1:], nil
	}

	return extProj.ExternalId, nil

}

func (self *SCloudprovider) GetSchedtags() []SSchedtag {
	return GetSchedtags(CloudproviderschedtagManager, self.Id)
}

func (self *SCloudprovider) GetDynamicConditionInput() *jsonutils.JSONDict {
	return jsonutils.Marshal(self).(*jsonutils.JSONDict)
}

func (self *SCloudprovider) PerformSetSchedtag(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, data jsonutils.JSONObject) (jsonutils.JSONObject, error) {
	return PerformSetResourceSchedtag(self, ctx, userCred, query, data)
}

func (self *SCloudprovider) GetSchedtagJointManager() ISchedtagJointManager {
	return CloudproviderschedtagManager
}

func (self *SCloudprovider) GetInterVpcNetworks() ([]SInterVpcNetwork, error) {
	networks := []SInterVpcNetwork{}
	q := InterVpcNetworkManager.Query().Equals("manager_id", self.Id)
	err := db.FetchModelObjects(InterVpcNetworkManager, q, &networks)
	if err != nil {
		return nil, errors.Wrapf(err, "db.FetchModelObjects")
	}
	return networks, nil

}

func (self *SCloudprovider) SyncInterVpcNetwork(ctx context.Context, userCred mcclient.TokenCredential, interVpcNetworks []cloudprovider.ICloudInterVpcNetwork) ([]SInterVpcNetwork, []cloudprovider.ICloudInterVpcNetwork, compare.SyncResult) {
	lockman.LockRawObject(ctx, self.Keyword(), fmt.Sprintf("%s-interVpcNetwork", self.Id))
	defer lockman.ReleaseRawObject(ctx, self.Keyword(), fmt.Sprintf("%s-interVpcNetwork", self.Id))

	result := compare.SyncResult{}

	localNetworks := []SInterVpcNetwork{}
	remoteNetworks := []cloudprovider.ICloudInterVpcNetwork{}

	dbNetworks, err := self.GetInterVpcNetworks()
	if err != nil {
		result.Error(errors.Wrapf(err, "GetInterVpcNetworks"))
		return nil, nil, result
	}

	removed := make([]SInterVpcNetwork, 0)
	commondb := make([]SInterVpcNetwork, 0)
	commonext := make([]cloudprovider.ICloudInterVpcNetwork, 0)
	added := make([]cloudprovider.ICloudInterVpcNetwork, 0)

	err = compare.CompareSets(dbNetworks, interVpcNetworks, &removed, &commondb, &commonext, &added)
	if err != nil {
		result.Error(err)
		return nil, nil, result
	}

	for i := 0; i < len(removed); i += 1 {
		err = removed[i].syncRemove(ctx, userCred)
		if err != nil {
			result.DeleteError(err)
			continue
		}
		result.Delete()
	}

	for i := 0; i < len(commondb); i += 1 {
		err = commondb[i].SyncWithCloudInterVpcNetwork(ctx, userCred, commonext[i])
		if err != nil {
			result.UpdateError(errors.Wrapf(err, "SyncWithCloudInterVpcNetwork"))
			continue
		}
		localNetworks = append(localNetworks, commondb[i])
		remoteNetworks = append(remoteNetworks, commonext[i])

		result.Update()
	}

	for i := 0; i < len(added); i += 1 {
		network, err := InterVpcNetworkManager.newFromCloudInterVpcNetwork(ctx, userCred, added[i], self)
		if err != nil {
			result.AddError(err)
			continue
		}

		localNetworks = append(localNetworks, *network)
		remoteNetworks = append(remoteNetworks, added[i])

		result.Add()
	}

	return localNetworks, remoteNetworks, result
}

func (manager *SCloudproviderManager) ListItemExportKeys(ctx context.Context, q *sqlchemy.SQuery, userCred mcclient.TokenCredential, keys stringutils2.SSortedStrings) (*sqlchemy.SQuery, error) {
	q, err := manager.SEnabledStatusStandaloneResourceBaseManager.ListItemExportKeys(ctx, q, userCred, keys)
	if err != nil {
		return nil, errors.Wrap(err, "SEnabledStatusStandaloneResourceBaseManager.ListItemExportKeys")
	}
	q, err = manager.SProjectizedResourceBaseManager.ListItemExportKeys(ctx, q, userCred, keys)
	if err != nil {
		return nil, errors.Wrapf(err, "SProjectizedResourceBaseManager.ListItemExportKeys")
	}
	q, err = manager.SProjectMappingResourceBaseManager.ListItemExportKeys(ctx, q, userCred, keys)
	if err != nil {
		return nil, errors.Wrapf(err, "SProjectMappingResourceBaseManager.ListItemExportKeys")
	}
	return q, nil
}

// 绑定同步策略
func (self *SCloudprovider) PerformProjectMapping(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input api.CloudaccountProjectMappingInput) (jsonutils.JSONObject, error) {
	if len(input.ProjectMappingId) > 0 {
		_, err := validators.ValidateModel(userCred, ProjectMappingManager, &input.ProjectMappingId)
		if err != nil {
			return nil, err
		}
		if len(self.ProjectMappingId) > 0 && self.ProjectMappingId != input.ProjectMappingId {
			return nil, httperrors.NewInputParameterError("cloudprovider %s has aleady bind project mapping %s", self.Name, self.ProjectMappingId)
		}
	}
	_, err := db.Update(self, func() error {
		self.ProjectMappingId = input.ProjectMappingId
		if input.EnableProjectSync != nil {
			self.EnableProjectSync = tristate.NewFromBool(*input.EnableProjectSync)
		}
		if input.EnableResourceSync != nil {
			self.EnableResourceSync = tristate.NewFromBool(*input.EnableResourceSync)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return nil, refreshPmCaches()
}

func (self *SCloudprovider) PerformSetSyncing(ctx context.Context, userCred mcclient.TokenCredential, query jsonutils.JSONObject, input api.CloudproviderSync) (jsonutils.JSONObject, error) {
	regionIds := []string{}
	for i := range input.CloudregionIds {
		_, err := validators.ValidateModel(userCred, CloudregionManager, &input.CloudregionIds[i])
		if err != nil {
			return nil, err
		}
		regionIds = append(regionIds, input.CloudregionIds[i])
	}
	if len(regionIds) == 0 {
		return nil, nil
	}
	q := CloudproviderRegionManager.Query().Equals("cloudprovider_id", self.Id).In("cloudregion_id", regionIds)
	cpcds := []SCloudproviderregion{}
	err := db.FetchModelObjects(CloudproviderRegionManager, q, &cpcds)
	if err != nil {
		return nil, err
	}
	for i := range cpcds {
		_, err := db.Update(&cpcds[i], func() error {
			cpcds[i].Enabled = input.Enabled
			return nil
		})
		if err != nil {
			return nil, errors.Wrapf(err, "db.Update")
		}
	}
	return nil, nil
}
