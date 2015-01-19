// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/log"
	rm "github.com/couchbase/goxdcr/replication_manager"
	"github.com/couchbase/goxdcr/service_def"
	"github.com/couchbase/goxdcr/service_impl"
	"github.com/couchbase/goxdcr/tests/common"
	"github.com/couchbase/goxdcr/utils"
	"io/ioutil"
	"os"
)

var logger *log.CommonLogger = log.NewLogger("capi_service", log.DefaultLoggerContext)

var options struct {
	sourceKVHost string //source kv host name
	sourceKVPort uint64 //source kv admin port

	username string //username
	password string //password

	remoteUuid             string // remote cluster uuid
	remoteName             string // remote cluster name
	remoteHostName         string // remote cluster host name
	remoteUserName         string //remote cluster userName
	remotePassword         string //remote cluster password
	remoteDemandEncryption bool   // whether encryption is needed
	remoteCertificateFile  string // file containing certificate for encryption

}

func argParse() {
	flag.StringVar(&options.sourceKVHost, "sourceKVHost", base.LocalHostName, "source kv host")
	flag.Uint64Var(&options.sourceKVPort, "sourceKVPort", 9000,
		"admin port number for source kv")
	flag.StringVar(&options.username, "username", "Administrator", "userName to cluster admin console")
	flag.StringVar(&options.password, "password", "welcome", "password to Cluster admin console")

	flag.StringVar(&options.remoteUuid, "remoteUuid", "1234567",
		"remote cluster uuid")
	flag.StringVar(&options.remoteName, "remoteName", "remote",
		"remote cluster name")
	flag.StringVar(&options.remoteHostName, "remoteHostName", "127.0.0.1:9000",
		"remote cluster host name")
	flag.StringVar(&options.remoteUserName, "remoteUserName", "Administrator", "remote cluster userName")
	flag.StringVar(&options.remotePassword, "remotePassword", "welcome", "remote cluster password")
	flag.BoolVar(&options.remoteDemandEncryption, "remoteDemandEncryption", false, "whether encryption is needed")
	flag.StringVar(&options.remoteCertificateFile, "remoteCertificateFile", "", "file containing certificate for encryption")

	flag.Parse()
}

func usage() {
	fmt.Fprintf(os.Stderr, "Usage : %s [OPTIONS] \n", os.Args[0])
	flag.PrintDefaults()
}

func main() {
	fmt.Println("Start Testing capi_service")
	argParse()
	err := test()
	if err != nil {
		panic(err)
	}
}

func test() error {
	err := setup()
	if err != nil {
		return err
	}
	defer teardown()
	err = run_testcase()
	if err != nil {
		return err
	}
	return nil
}

func setup() error {
	//create remote cluster reference
	err := createRemoteCluster()
	if err != nil {
		return err
	}
	logger.Infof("created remote cluster %v\n", options.remoteName)
	return nil
}
func teardown() error {
	//delete remote cluster reference
	err := deleteRemoteCluster()
	if err != nil {
		return err
	}
	return nil
}

func run_testcase() error {
	metadata_svc, err := service_impl.NewMetaKVMetadataSvc(log.DefaultLoggerContext)
	if err != nil {
		return err
	}

	remote_cluster_svc := service_impl.NewRemoteClusterService(metadata_svc, log.DefaultLoggerContext)
	if err != nil {
		return err
	}

	capi_svc := service_impl.NewCAPIService(log.DefaultLoggerContext)
	if err != nil {
		return err
	}

	remoteBucket, err := service_def.NewRemoteBucketInfo(options.remoteName, "default", nil, remote_cluster_svc, logger)
	if err != nil {
		return err
	}
	var vbno uint16 = 0
	vbuuid, err := testPreReplicate(capi_svc, remoteBucket, vbno)
	if err != nil {
		return errors.New(fmt.Sprintf("testPreReplicate failed - err=%v", err))
	}

	remote_seqno, vbuuid_1, err := testCommitForCheckpoint(capi_svc, remoteBucket, vbuuid)
	if err != nil {
		return errors.New(fmt.Sprintf("testCommitForCheckpoint failed - err=%v", err))
	}

	if vbuuid_1 != 0 {
		vbuuid = vbuuid_1
	}
	logger.Infof("vb=%v, remote_seqno=%v, vb_uuid=%v\n", vbno, remote_seqno, vbuuid)

	remoteVBUUIDs := [][]uint64{}
	vb0_pair := []uint64{0, vbuuid}
	remoteVBUUIDs = append(remoteVBUUIDs, vb0_pair)
	err = testMassValidateVBUUIDs(capi_svc, remoteBucket, remoteVBUUIDs)
	if err != nil {
		return errors.New(fmt.Sprintf("testMassValidateVBUUIDs failed - err=%v", err))
	}

	return nil
}

func testPreReplicate(capi_svc *service_impl.CAPIService, remoteBucket *service_def.RemoteBucketInfo, vbno uint16) (uint64, error) {

	knownRemoteVBStatus := &service_def.RemoteVBReplicationStatus{VBNo: vbno, VBUUID: 0, VBSeqno: 0}
	bVBMatch, current_remoteVBUUID, err := capi_svc.PreReplicate(remoteBucket,
		knownRemoteVBStatus, true)

	logger.Infof("_pre_replicate returns for vb=%v:\n", knownRemoteVBStatus.VBNo)
	logger.Infof("bVBMatch=%v, current_remteVBUUID=%v, err=%v\n", bVBMatch, current_remoteVBUUID, err)
	if err != nil {
		return 0, err
	}
	return current_remoteVBUUID, nil
}

func testCommitForCheckpoint(capi_svc *service_impl.CAPIService, remoteBucket *service_def.RemoteBucketInfo, vbuuid uint64) (remote_seqno uint64, vb_uuid uint64, err error) {
	remote_seqno, vb_uuid, err = capi_svc.CommitForCheckpoint(remoteBucket, vbuuid, 0)
	return
}

func testMassValidateVBUUIDs(capi_svc *service_impl.CAPIService, remoteBucket *service_def.RemoteBucketInfo, remoteVBUUIDs [][]uint64) error {
	matching, mismatching, missing, err := capi_svc.MassValidateVBUUIDs(remoteBucket, remoteVBUUIDs)

	if err != nil {
		return err
	}

	if len(matching) == 0 || len(mismatching) > 0 || len(missing) > 0 {
		return errors.New(fmt.Sprintf("matching=%v, mismatching=%v, missing=%v", matching, mismatching, missing))
	}
	return nil
}

//helper functions for setup
func createRemoteCluster() error {
	fmt.Println("Starting createRemoteCluster")
	url := common.GetAdminportUrlPrefix(options.sourceKVHost, options.sourceKVPort)
	paramsBytes, err := createRequestBody(options.remoteName, options.remoteHostName, options.remoteUserName,
		options.remotePassword, options.remoteDemandEncryption, options.remoteCertificateFile)
	if err != nil {
		return err
	}

	var respMap map[string]interface{}
	err, _ = utils.QueryRestApiWithAuth(url,
		base.RemoteClustersPath,
		options.username,
		options.password,
		base.MethodPost,
		"",
		paramsBytes,
		respMap,
		logger, nil)

	if err != nil {
		return err
	} else {
		if errList, ok := respMap[rm.ErrorsKey]; ok {
			return errors.New(fmt.Sprintf("Failed to create remote cluster reference, err=%v", errList))
		}
	}

	return nil
}

func createRequestBody(name, hostname, username, password string, demandEncryption bool, certificateFile string) ([]byte, error) {

	params := make(map[string]interface{})
	params[base.RemoteClusterName] = name
	params[base.RemoteClusterHostName] = hostname
	params[base.RemoteClusterUserName] = username
	params[base.RemoteClusterPassword] = password
	params[base.RemoteClusterDemandEncryption] = demandEncryption

	// read certificate from file
	if certificateFile != "" {
		serverCert, err := ioutil.ReadFile(certificateFile)
		if err != nil {
			fmt.Printf("Could not load server certificate! err=%v\n", err)
			return nil, err
		}
		params[base.RemoteClusterCertificate] = serverCert
	}
	return rm.EncodeMapIntoByteArray(params)
}

func deleteRemoteCluster() error {
	fmt.Println("Starting DeleteRemoteCluster")
	url := common.GetAdminportUrlPrefix(options.sourceKVHost, options.sourceKVPort)
	err, _ := utils.QueryRestApiWithAuth(url,
		base.RemoteClustersPath+base.UrlDelimiter+options.remoteName,
		options.username,
		options.password,
		base.MethodDelete,
		"",
		nil,
		nil,
		logger, nil)

	if err != nil {
		return err
	}

	return nil
}

func getRemoteBucketInfo(remoteRefName string, bucketName string) *service_def.RemoteBucketInfo {

	remoteBucket := &service_def.RemoteBucketInfo{RemoteClusterRefName: remoteRefName,
		BucketName: bucketName}

	return remoteBucket

}