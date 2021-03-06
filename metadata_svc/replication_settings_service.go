// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package metadata_svc

import (
	"encoding/json"
	"github.com/couchbase/goxdcr/log"
	"github.com/couchbase/goxdcr/metadata"
	"github.com/couchbase/goxdcr/service_def"
	"sync"
)

var DefaultReplicationSettingsKey = "DefaultReplicationSettings"

type ReplicationSettingsSvc struct {
	metadata_svc           service_def.MetadataSvc
	logger                 *log.CommonLogger
	replicationSettingsMtx sync.Mutex
}

func NewReplicationSettingsSvc(metadata_svc service_def.MetadataSvc, logger_ctx *log.LoggerContext) *ReplicationSettingsSvc {
	return &ReplicationSettingsSvc{
		metadata_svc: metadata_svc,
		logger:       log.NewLogger("ReplSettSvc", logger_ctx),
	}
}

func (repl_settings_svc *ReplicationSettingsSvc) GetDefaultReplicationSettings() (*metadata.ReplicationSettings, error) {
	var defaultSettings *metadata.ReplicationSettings
	repl_settings_svc.replicationSettingsMtx.Lock()
	defer repl_settings_svc.replicationSettingsMtx.Unlock()

	bytes, rev, err := repl_settings_svc.metadata_svc.Get(DefaultReplicationSettingsKey)
	if err != nil && err != service_def.MetadataNotFoundErr {
		return nil, err
	}
	if err == service_def.MetadataNotFoundErr {
		// initialize default settings if it does not exist
		defaultSettings = metadata.DefaultReplicationSettings()
	} else {
		defaultSettings, err = repl_settings_svc.constructReplicationSettingsObject(bytes, rev)
		if err != nil {
			return nil, err
		}
		// set rev number
		defaultSettings.Revision = rev
	}
	return defaultSettings, nil
}

func (repl_settings_svc *ReplicationSettingsSvc) SetDefaultReplicationSettings(settings *metadata.ReplicationSettings) error {
	repl_settings_svc.replicationSettingsMtx.Lock()
	defer repl_settings_svc.replicationSettingsMtx.Unlock()
	bytes, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	if settings.Revision != nil {
		return repl_settings_svc.metadata_svc.Set(DefaultReplicationSettingsKey, bytes, settings.Revision)
	} else {
		return repl_settings_svc.metadata_svc.Add(DefaultReplicationSettingsKey, bytes)
	}
}

func (repl_settings_svc *ReplicationSettingsSvc) constructReplicationSettingsObject(value []byte, rev interface{}) (*metadata.ReplicationSettings, error) {
	settings := &metadata.ReplicationSettings{}
	err := json.Unmarshal(value, settings)
	if err != nil {
		return nil, err
	}
	settings.Revision = rev

	settings.PostProcessAfterUnmarshalling()
	return settings, nil
}
