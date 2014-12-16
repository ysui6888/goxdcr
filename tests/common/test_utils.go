package common

import (
	"net/http"
	"fmt"
	"errors"
	"io/ioutil"
	"reflect"
	"bytes"
	rm "github.com/couchbase/goxdcr/replication_manager"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/utils"
	"github.com/couchbase/goxdcr/metadata"
	"github.com/couchbase/goxdcr/service_def"
)

func GetAdminportUrlPrefix(hostName string) string {
	return "http://" + utils.GetHostAddr(hostName, base.AdminportNumber) + base.AdminportUrlPrefix
}

func ValidateResponse(testName string, response *http.Response, err error) error {
	if err != nil || response.StatusCode != 200 {
		errMsg := fmt.Sprintf("Test %v failed. err=%v", testName, err)
		if response != nil {
			errMsg += fmt.Sprintf("; response status=%v", response.Status)
		}
		errMsg += "\n"
		return errors.New(errMsg)
	}
	return nil
}

func ValidateFieldValue(fieldName string, expectedValue, actualValue interface{}) error {
	if expectedValue != actualValue {
		return errors.New(fmt.Sprintf("Incorrect value in field %v. Expected value=%v with type=%v, actual value=%v with type=%v\n", 
									fieldName, expectedValue, reflect.TypeOf(expectedValue), actualValue, reflect.TypeOf(actualValue)))
	}
	return nil
}

func CreateTestRemoteCluster(remote_cluster_service service_def.RemoteClusterSvc, remoteUuid, remoteName, remoteHostName, remoteUserName, remotePassword string, 
                             remoteDemandEncryption bool, remoteCertificateFile string) error {
	var serverCert []byte
	var err error
	// read certificate from file
	if remoteCertificateFile != "" {
		serverCert, err = ioutil.ReadFile(remoteCertificateFile)
		if err != nil {
    		fmt.Printf("Could not load server certificate! err=%v\n", err)
    		return err
		}
	}
	
	remoteClusterRef := metadata.NewRemoteClusterReference(remoteUuid, remoteName, remoteHostName, remoteUserName, remotePassword, remoteDemandEncryption, serverCert)
	err = remote_cluster_service.AddRemoteCluster(remoteClusterRef)
	fmt.Printf("Added remote cluster reference with name=%v, err=%v\n", remoteName, err)
	return err
}

func DeleteTestRemoteCluster(remote_cluster_service service_def.RemoteClusterSvc, remoteName string) error {
	err := remote_cluster_service.DelRemoteCluster(remoteName)
	fmt.Printf("Deleted remote cluster reference with name=%v, err=%v\n", remoteName, err)
	return err
}

func SendRequestAndValidateResponse(testName, httpMethod, url string, body []byte) (*http.Response, error) {
	request, err := http.NewRequest(httpMethod, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set(rm.ContentType, rm.DefaultContentType)

	fmt.Println("request", request)

	response, err := http.DefaultClient.Do(request)
	err = ValidateResponse(testName, response, err)
	fmt.Printf("err=%v, response=%v\n", err, response)
	return response, err
}
