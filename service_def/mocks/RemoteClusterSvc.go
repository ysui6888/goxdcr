// Code generated by mockery v1.0.0
package mocks

import base "github.com/couchbase/goxdcr/base"
import metadata "github.com/couchbase/goxdcr/metadata"
import mock "github.com/stretchr/testify/mock"

// RemoteClusterSvc is an autogenerated mock type for the RemoteClusterSvc type
type RemoteClusterSvc struct {
	mock.Mock
}

// AddRemoteCluster provides a mock function with given fields: ref, skipConnectivityValidation
func (_m *RemoteClusterSvc) AddRemoteCluster(ref *metadata.RemoteClusterReference, skipConnectivityValidation bool) error {
	ret := _m.Called(ref, skipConnectivityValidation)

	var r0 error
	if rf, ok := ret.Get(0).(func(*metadata.RemoteClusterReference, bool) error); ok {
		r0 = rf(ref, skipConnectivityValidation)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// CheckAndUnwrapRemoteClusterError provides a mock function with given fields: err
func (_m *RemoteClusterSvc) CheckAndUnwrapRemoteClusterError(err error) (bool, error) {
	ret := _m.Called(err)

	var r0 bool
	if rf, ok := ret.Get(0).(func(error) bool); ok {
		r0 = rf(err)
	} else {
		r0 = ret.Get(0).(bool)
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(error) error); ok {
		r1 = rf(err)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// DelRemoteCluster provides a mock function with given fields: refName
func (_m *RemoteClusterSvc) DelRemoteCluster(refName string) (*metadata.RemoteClusterReference, error) {
	ret := _m.Called(refName)

	var r0 *metadata.RemoteClusterReference
	if rf, ok := ret.Get(0).(func(string) *metadata.RemoteClusterReference); ok {
		r0 = rf(refName)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*metadata.RemoteClusterReference)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string) error); ok {
		r1 = rf(refName)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetConnectionStringForRemoteCluster provides a mock function with given fields: ref, isCapiReplication
func (_m *RemoteClusterSvc) GetConnectionStringForRemoteCluster(ref *metadata.RemoteClusterReference, isCapiReplication bool) (string, error) {
	ret := _m.Called(ref, isCapiReplication)

	var r0 string
	if rf, ok := ret.Get(0).(func(*metadata.RemoteClusterReference, bool) string); ok {
		r0 = rf(ref, isCapiReplication)
	} else {
		r0 = ret.Get(0).(string)
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(*metadata.RemoteClusterReference, bool) error); ok {
		r1 = rf(ref, isCapiReplication)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// GetRemoteClusterNameFromClusterUuid provides a mock function with given fields: uuid
func (_m *RemoteClusterSvc) GetRemoteClusterNameFromClusterUuid(uuid string) string {
	ret := _m.Called(uuid)

	var r0 string
	if rf, ok := ret.Get(0).(func(string) string); ok {
		r0 = rf(uuid)
	} else {
		r0 = ret.Get(0).(string)
	}

	return r0
}

// RemoteClusterByRefId provides a mock function with given fields: refId, refresh
func (_m *RemoteClusterSvc) RemoteClusterByRefId(refId string, refresh bool) (*metadata.RemoteClusterReference, error) {
	ret := _m.Called(refId, refresh)

	var r0 *metadata.RemoteClusterReference
	if rf, ok := ret.Get(0).(func(string, bool) *metadata.RemoteClusterReference); ok {
		r0 = rf(refId, refresh)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*metadata.RemoteClusterReference)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, bool) error); ok {
		r1 = rf(refId, refresh)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// RemoteClusterByRefName provides a mock function with given fields: refName, refresh
func (_m *RemoteClusterSvc) RemoteClusterByRefName(refName string, refresh bool) (*metadata.RemoteClusterReference, error) {
	ret := _m.Called(refName, refresh)

	var r0 *metadata.RemoteClusterReference
	if rf, ok := ret.Get(0).(func(string, bool) *metadata.RemoteClusterReference); ok {
		r0 = rf(refName, refresh)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*metadata.RemoteClusterReference)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, bool) error); ok {
		r1 = rf(refName, refresh)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// RemoteClusterByUuid provides a mock function with given fields: uuid, refresh
func (_m *RemoteClusterSvc) RemoteClusterByUuid(uuid string, refresh bool) (*metadata.RemoteClusterReference, error) {
	ret := _m.Called(uuid, refresh)

	var r0 *metadata.RemoteClusterReference
	if rf, ok := ret.Get(0).(func(string, bool) *metadata.RemoteClusterReference); ok {
		r0 = rf(uuid, refresh)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*metadata.RemoteClusterReference)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, bool) error); ok {
		r1 = rf(uuid, refresh)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// RemoteClusterServiceCallback provides a mock function with given fields: path, value, rev
func (_m *RemoteClusterSvc) RemoteClusterServiceCallback(path string, value []byte, rev interface{}) error {
	ret := _m.Called(path, value, rev)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, []byte, interface{}) error); ok {
		r0 = rf(path, value, rev)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// RemoteClusters provides a mock function with given fields: refresh
func (_m *RemoteClusterSvc) RemoteClusters(refresh bool) (map[string]*metadata.RemoteClusterReference, error) {
	ret := _m.Called(refresh)

	var r0 map[string]*metadata.RemoteClusterReference
	if rf, ok := ret.Get(0).(func(bool) map[string]*metadata.RemoteClusterReference); ok {
		r0 = rf(refresh)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(map[string]*metadata.RemoteClusterReference)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(bool) error); ok {
		r1 = rf(refresh)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// SetMetadataChangeHandlerCallback provides a mock function with given fields: callBack
func (_m *RemoteClusterSvc) SetMetadataChangeHandlerCallback(callBack base.MetadataChangeHandlerCallback) {
	_m.Called(callBack)
}

// SetRemoteCluster provides a mock function with given fields: refName, ref
func (_m *RemoteClusterSvc) SetRemoteCluster(refName string, ref *metadata.RemoteClusterReference) error {
	ret := _m.Called(refName, ref)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *metadata.RemoteClusterReference) error); ok {
		r0 = rf(refName, ref)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// ValidateAddRemoteCluster provides a mock function with given fields: ref
func (_m *RemoteClusterSvc) ValidateAddRemoteCluster(ref *metadata.RemoteClusterReference) error {
	ret := _m.Called(ref)

	var r0 error
	if rf, ok := ret.Get(0).(func(*metadata.RemoteClusterReference) error); ok {
		r0 = rf(ref)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// ValidateRemoteCluster provides a mock function with given fields: ref
func (_m *RemoteClusterSvc) ValidateRemoteCluster(ref *metadata.RemoteClusterReference) error {
	ret := _m.Called(ref)

	var r0 error
	if rf, ok := ret.Get(0).(func(*metadata.RemoteClusterReference) error); ok {
		r0 = rf(ref)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// ValidateSetRemoteCluster provides a mock function with given fields: refName, ref
func (_m *RemoteClusterSvc) ValidateSetRemoteCluster(refName string, ref *metadata.RemoteClusterReference) error {
	ret := _m.Called(refName, ref)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, *metadata.RemoteClusterReference) error); ok {
		r0 = rf(refName, ref)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}
