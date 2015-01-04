// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.
 
package service_impl

import (
	"fmt"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/log"
	"github.com/couchbase/goxdcr/utils"
	"github.com/couchbaselabs/go-couchbase"
)

type ClusterInfoSvc struct {
	logger      *log.CommonLogger
}

func NewClusterInfoSvc(logger_ctx *log.LoggerContext) *ClusterInfoSvc {
	return &ClusterInfoSvc{
					logger:    log.NewLogger("ClusterInfoService", logger_ctx),
					}
}

func (ci_svc *ClusterInfoSvc) GetMyActiveVBuckets(clusterConnInfoProvider base.ClusterConnectionInfoProvider, bucketName, NodeId string) ([]uint16, error) {
	bucket, err := ci_svc.GetBucket(clusterConnInfoProvider, bucketName)	
	if err != nil {
		return nil, err
	}
	defer bucket.Close()

	// in test env, there should be only one kv in bucket server list
	kvaddr := bucket.VBServerMap().ServerList[0]

	m, err := bucket.GetVBmap([]string{kvaddr})
	if err != nil {
		return nil, err
	}

	vbList := m[kvaddr]
	

	return vbList, nil
}

func (ci_svc *ClusterInfoSvc) GetServerList(clusterConnInfoProvider base.ClusterConnectionInfoProvider, bucketName string) ([]string, error) {
	bucket, err := ci_svc.GetBucket(clusterConnInfoProvider, bucketName)	
	if err != nil {
		return nil, err
	}
	defer bucket.Close()

	// in test env, there should be only one kv in bucket server list
	serverlist := bucket.VBServerMap().ServerList
	
	return serverlist, nil
}

func (ci_svc *ClusterInfoSvc) GetServerVBucketsMap(clusterConnInfoProvider base.ClusterConnectionInfoProvider, bucketName string) (map[string][]uint16, error) {
	bucket, err := ci_svc.GetBucket(clusterConnInfoProvider, bucketName)
	if err != nil {
		return nil, err
	}
	defer bucket.Close()
	
	fmt.Printf("ServerList=%v\n", bucket.VBServerMap().ServerList)
	serverVBMap, err := bucket.GetVBmap(bucket.VBServerMap().ServerList)

	return serverVBMap, err
}

func (ci_svc *ClusterInfoSvc) IsNodeCompatible(node string, version string) (bool, error) {
	return true, nil
}

func (ci_svc *ClusterInfoSvc) GetBucket(clusterConnInfoProvider base.ClusterConnectionInfoProvider, bucketName string) (*couchbase.Bucket, error) {
	return utils.Bucket(clusterConnInfoProvider.MyConnectionStr(), bucketName, clusterConnInfoProvider.MyUsername(), clusterConnInfoProvider.MyPassword())
}