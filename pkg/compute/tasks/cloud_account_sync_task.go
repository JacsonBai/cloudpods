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

package tasks

import (
	"context"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"

	"yunion.io/x/onecloud/pkg/cloudcommon/db"
	"yunion.io/x/onecloud/pkg/cloudcommon/db/taskman"
	"yunion.io/x/onecloud/pkg/compute/models"
	"yunion.io/x/onecloud/pkg/httperrors"
	"yunion.io/x/onecloud/pkg/util/logclient"
)

type CloudAccountSyncInfoTask struct {
	taskman.STask
}

func init() {
	taskman.RegisterTask(CloudAccountSyncInfoTask{})
}

func (self *CloudAccountSyncInfoTask) OnInit(ctx context.Context, obj db.IStandaloneModel, body jsonutils.JSONObject) {
	cloudaccount := obj.(*models.SCloudaccount)

	db.OpsLog.LogEvent(cloudaccount, db.ACT_SYNCING_HOST, "", self.UserCred)
	// cloudaccount.MarkSyncing(self.UserCred)

	self.SetStage("OnCloudaccountSyncReady", nil)

	taskman.LocalTaskRun(self, func() (jsonutils.JSONObject, error) {
		// do sync
		err := cloudaccount.SyncCallSyncAccountTask(ctx, self.UserCred)
		if err != nil {
			if errors.Cause(err) == httperrors.ErrConflict {
				log.Errorf("account %s(%s) alread in syncing", cloudaccount.Name, cloudaccount.Provider)
			}
			// 进入同步任务前已经mark sync, 这里需要清理下状态
			cloudaccount.MarkEndSyncWithLock(ctx, self.UserCred)
			return nil, errors.Wrap(err, "SyncCallSyncAccountTask")
		}
		return nil, nil
	})
}

func (self *CloudAccountSyncInfoTask) OnCloudaccountSyncReadyFailed(ctx context.Context, obj db.IStandaloneModel, err jsonutils.JSONObject) {
	cloudaccount := obj.(*models.SCloudaccount)
	db.OpsLog.LogEvent(cloudaccount, db.ACT_SYNC_HOST_FAILED, err, self.UserCred)
	self.SetStageFailed(ctx, err)
	logclient.AddActionLogWithStartable(self, cloudaccount, logclient.ACT_CLOUD_SYNC, err, self.UserCred, false)
}

func (self *CloudAccountSyncInfoTask) OnCloudaccountSyncReady(ctx context.Context, obj db.IStandaloneModel, body jsonutils.JSONObject) {
	cloudaccount := obj.(*models.SCloudaccount)

	syncRange := models.SSyncRange{}
	syncRangeJson, _ := self.Params.Get("sync_range")
	if syncRangeJson != nil {
		syncRangeJson.Unmarshal(&syncRange)
	} else {
		syncRange.FullSync = true
		syncRange.DeepSync = true
	}

	if !syncRange.NeedSyncInfo() {
		self.OnCloudaccountSyncComplete(ctx, obj, nil)
		return
	}

	err := models.SyncCloudaccountResources(ctx, self.GetUserCred(), cloudaccount, &syncRange)
	if err != nil {
		log.Errorf("SyncAccountResources error: %v", err)
	}

	cloudproviders := cloudaccount.GetEnabledCloudproviders()

	if len(cloudproviders) > 0 {
		self.SetStage("on_cloudaccount_sync_complete", nil)
		for i := range cloudproviders {
			cloudproviders[i].StartSyncCloudProviderInfoTask(ctx, self.UserCred, &syncRange, self.GetId())
		}
	} else {
		self.OnCloudaccountSyncComplete(ctx, obj, nil)
	}
}

func (self *CloudAccountSyncInfoTask) OnCloudaccountSyncComplete(ctx context.Context, obj db.IStandaloneModel, data jsonutils.JSONObject) {
	cloudaccount := obj.(*models.SCloudaccount)
	cloudaccount.MarkEndSyncWithLock(ctx, self.UserCred)
	db.OpsLog.LogEvent(cloudaccount, db.ACT_SYNC_HOST_COMPLETE, "", self.UserCred)
	self.SetStageComplete(ctx, nil)
	logclient.AddActionLogWithStartable(self, cloudaccount, logclient.ACT_CLOUD_SYNC, "", self.UserCred, true)
}

func (self *CloudAccountSyncInfoTask) OnCloudaccountSyncCompleteFailed(ctx context.Context, obj db.IStandaloneModel, err jsonutils.JSONObject) {
	cloudaccount := obj.(*models.SCloudaccount)
	cloudaccount.MarkEndSyncWithLock(ctx, self.UserCred)
	db.OpsLog.LogEvent(cloudaccount, db.ACT_SYNC_HOST_FAILED, err, self.UserCred)
	self.SetStageFailed(ctx, err)
	logclient.AddActionLogWithStartable(self, cloudaccount, logclient.ACT_CLOUD_SYNC, err, self.UserCred, false)
}
