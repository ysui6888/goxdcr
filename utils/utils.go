package utils

import (
	"bytes"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"github.com/couchbase/cbauth"
	"github.com/couchbase/go-couchbase"
	"github.com/couchbase/gojsonsm"
	mc "github.com/couchbase/gomemcached"
	mcc "github.com/couchbase/gomemcached/client"
	"github.com/couchbase/goutils/scramsha"
	base "github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/log"
	"github.com/couchbase/goxdcr/metadata"
	"github.com/golang/snappy"
	gocb "gopkg.in/couchbase/gocb.v1"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

var NonExistentBucketError error = errors.New("Bucket doesn't exist")
var TargetMayNotSupportScramShaError error = errors.New("Target may not support ScramSha")

func (f *HELOFeatures) NumberOfActivatedFeatures() int {
	var result int
	if f.Xattribute {
		result++
	}
	if f.CompressionType != base.CompressionTypeNone {
		result++
	}
	return result
}

type Utilities struct {
	logger_utils *log.CommonLogger
}

/**
 * NOTE - ideally we want to be able to pass in utility interfaces so we can do much
 * better unit testing with mocks. This constructor should be used in main() and then
 * passed down level by levels.
 * Currently, this method is being called in many places, and each place that is using
 * this method should ideally be using a passed in interface from another parent level.
 */
func NewUtilities() *Utilities {
	retVar := &Utilities{
		logger_utils: log.NewLogger("Utils", log.DefaultLoggerContext),
	}
	return retVar
}

/**
 * This utils file contains both regular non-REST related utilities as well as REST related.
 * The first section is non-REST related utility functions
 */

type BucketBasicStats struct {
	ItemCount int `json:"itemCount"`
}

//Only used by unit test
//TODO: replace with go-couchbase bucket stats API
type CouchBucket struct {
	Name string           `json:"name"`
	Stat BucketBasicStats `json:"basicStats"`
}

func (u *Utilities) GetNonExistentBucketError() error {
	return NonExistentBucketError
}

func (u *Utilities) loggerForFunc(logger *log.CommonLogger) *log.CommonLogger {
	var l *log.CommonLogger
	if logger != nil {
		l = logger
	} else {
		l = u.logger_utils
	}
	return l
}

func (u *Utilities) ValidateSettings(defs base.SettingDefinitions,
	settings metadata.ReplicationSettingsMap,
	logger *log.CommonLogger) error {
	var l *log.CommonLogger = u.loggerForFunc(logger)

	if l.GetLogLevel() >= log.LogLevelDebug {
		l.Debugf("Start validate setting=%v, defs=%v", settings.CloneAndRedact(), defs)
	}
	var err *base.SettingsError = nil
	for key, def := range defs {
		val, ok := settings[key]
		if !ok && def.Required {
			if err == nil {
				err = base.NewSettingsError()
			}
			err.Add(key, errors.New("required, but not supplied"))
		} else {
			if val != nil && def.Data_type != reflect.PtrTo(reflect.TypeOf(val)) {
				if err == nil {
					err = base.NewSettingsError()
				}
				err.Add(key, errors.New(fmt.Sprintf("expected type is %v, supplied type is %v",
					def.Data_type, reflect.TypeOf(val))))
			}
		}
	}
	if err != nil {
		l.Infof("setting validation result = %v", *err)
		return *err
	}
	return nil
}

func (u *Utilities) RecoverPanic(err *error) {
	if r := recover(); r != nil {
		*err = errors.New(fmt.Sprint(r))
	}
}

func (u *Utilities) LocalPool(localConnectStr string) (couchbase.Pool, error) {
	localURL := fmt.Sprintf("http://%s", localConnectStr)
	client, err := couchbase.ConnectWithAuth(localURL, cbauth.NewAuthHandler(nil))
	if err != nil {
		return couchbase.Pool{}, u.NewEnhancedError(fmt.Sprintf("Error connecting to couchbase. url=%v", u.UrlForLog(localURL)), err)
	}
	return client.GetPool("default")
}

// Get bucket in local cluster
func (u *Utilities) LocalBucket(localConnectStr, bucketName string) (*couchbase.Bucket, error) {
	u.logger_utils.Debugf("Getting local bucket name=%v\n", bucketName)

	pool, err := u.LocalPool(localConnectStr)
	if err != nil {
		return nil, err
	}

	bucket, err := pool.GetBucket(bucketName)
	if err != nil {
		return nil, u.NewEnhancedError(fmt.Sprintf("Error getting bucket, %v, from pool.", bucketName), err)
	}

	u.logger_utils.Debugf("Got local bucket successfully name=%v\n", bucket.Name)
	return bucket, err
}

func (u *Utilities) UnwrapError(infos map[string]interface{}) (err error) {
	if infos != nil && len(infos) > 0 {
		err = infos["error"].(error)
	}
	return err
}

// returns an enhanced error with erroe message being "msg + old error message"
func (u *Utilities) NewEnhancedError(msg string, err error) error {
	return errors.New(msg + "\n err = " + err.Error())
}

func (u *Utilities) GetMapFromExpvarMap(expvarMap *expvar.Map) map[string]interface{} {
	regMap := make(map[string]interface{})

	expvarMap.Do(func(keyValue expvar.KeyValue) {
		valueStr := keyValue.Value.String()
		// first check if valueStr is an integer
		valueInt, err := strconv.Atoi(valueStr)
		if err == nil {
			regMap[keyValue.Key] = valueInt
		} else {
			// then check if valueStr is a float
			valueFloat, err := strconv.ParseFloat(valueStr, 64)
			if err == nil {
				regMap[keyValue.Key] = valueFloat
			} else {
				// should never happen
				u.logger_utils.Errorf("Invalid value in expvarMap. Only float and integer values are supported")
			}
		}
	})
	return regMap
}

//convert the format returned by go-memcached StatMap - map[string]string to map[uint16]uint64
func (u *Utilities) ParseHighSeqnoStat(vbnos []uint16, stats_map map[string]string, highseqno_map map[uint16]uint64) error {
	var err error
	for _, vbno := range vbnos {
		stats_key := fmt.Sprintf(base.VBUCKET_HIGH_SEQNO_STAT_KEY_FORMAT, vbno)
		highseqnostr, ok := stats_map[stats_key]
		if !ok || highseqnostr == "" {
			err = fmt.Errorf("Can't find high seqno for vbno=%v in stats map. Source topology may have changed.\n", vbno)
			return err
		}
		highseqno, err := strconv.ParseUint(highseqnostr, 10, 64)
		if err != nil {
			u.logger_utils.Warnf("high seqno for vbno=%v in stats map is not a valid uint64. high seqno=%v\n", vbno, highseqnostr)
			err = fmt.Errorf("high seqno for vbno=%v in stats map is not a valid uint64. high seqno=%v\n", vbno, highseqnostr)
			return err
		}
		highseqno_map[vbno] = highseqno
	}
	return nil
}

//convert the format returned by go-memcached StatMap - map[string]string to map[uint16][]uint64
func (u *Utilities) ParseHighSeqnoAndVBUuidFromStats(vbnos []uint16, stats_map map[string]string, high_seqno_and_vbuuid_map map[uint16][]uint64) {
	for _, vbno := range vbnos {
		high_seqno_stats_key := fmt.Sprintf(base.VBUCKET_HIGH_SEQNO_STAT_KEY_FORMAT, vbno)
		highseqnostr, ok := stats_map[high_seqno_stats_key]
		if !ok {
			u.logger_utils.Warnf("Can't find high seqno for vbno=%v in stats map. Source topology may have changed.\n", vbno)
			continue
		}
		high_seqno, err := strconv.ParseUint(highseqnostr, 10, 64)
		if err != nil {
			u.logger_utils.Warnf("high seqno for vbno=%v in stats map is not a valid uint64. high seqno=%v\n", vbno, highseqnostr)
			continue
		}

		vbuuid_stats_key := fmt.Sprintf(base.VBUCKET_UUID_STAT_KEY_FORMAT, vbno)
		vbuuidstr, ok := stats_map[vbuuid_stats_key]
		if !ok {
			u.logger_utils.Warnf("Can't find vbuuid for vbno=%v in stats map. Source topology may have changed.\n", vbno)
			continue
		}
		vbuuid, err := strconv.ParseUint(vbuuidstr, 10, 64)
		if err != nil {
			u.logger_utils.Warnf("vbuuid for vbno=%v in stats map is not a valid uint64. vbuuid=%v\n", vbno, vbuuidstr)
			continue
		}

		high_seqno_and_vbuuid_map[vbno] = []uint64{high_seqno, vbuuid}
	}
}

// encode data in a map into a byte array, which can then be used as
// the body part of a http request
// so far only five types are supported: string, int, bool, LogLevel, []byte
// which should be sufficient for all cases at hand
func (u *Utilities) EncodeMapIntoByteArray(data map[string]interface{}) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	params := make(url.Values)
	for key, val := range data {
		var strVal string
		switch val.(type) {
		case string:
			strVal = val.(string)
		case int:
			strVal = strconv.FormatInt(int64(val.(int)), base.ParseIntBase)
		case bool:
			strVal = strconv.FormatBool(val.(bool))
		case log.LogLevel:
			strVal = val.(log.LogLevel).String()
		case []byte:
			strVal = string(val.([]byte))
		default:
			return nil, base.IncorrectValueTypeInMapError(key, val, "string/int/bool/LogLevel/[]byte")
		}
		params.Add(key, strVal)
	}

	return []byte(params.Encode()), nil
}

func (u *Utilities) UrlForLog(urlStr string) string {
	result, err := url.Parse(urlStr)
	if err == nil {
		if result.User != nil {
			result.User = url.UserPassword(result.User.Username(), "xxxx")
		}
		return result.String()
	} else {
		return urlStr
	}
}

func filterExpressionGetXattrHelper(bucket *gocb.Bucket, docId string, docCas gocb.Cas) ([]byte, error) {
	var xattrMap map[string]interface{}
	var xtoc interface{}
	var xattrSlice []byte

	xattrMap = make(map[string]interface{})
	frag, err := bucket.LookupIn(docId).GetEx(base.XattributeToc, gocb.SubdocFlagXattr).Execute()
	if err != nil {
		return nil, err
	}

	if frag.Cas() != docCas {
		return nil, base.ErrorInvalidCAS
	}

	err = frag.Content(base.XattributeToc, &xtoc)
	if err != nil {
		return nil, err
	}

	tocList := xtoc.([]interface{})
	for _, tocEntry := range tocList {
		if entry, ok := tocEntry.(string); ok {
			frag, err := bucket.LookupIn(docId).GetEx(entry, gocb.SubdocFlagXattr).Execute()
			if err != nil {
				return nil, err
			}

			if frag.Cas() != docCas {
				return nil, base.ErrorInvalidCAS
			}

			var value interface{}
			frag.Content(entry, &value)
			xattrMap[entry] = value
		}
	}
	xattrSlice, err = json.Marshal(xattrMap)
	if err != nil {
		return nil, err
	}

	return xattrSlice, nil
}

func filterExpressionGetDocVal(bucket *gocb.Bucket, docId string) ([]byte, gocb.Cas, error) {
	var retrievedDocVal interface{}
	docCas, err := bucket.Get(docId, &retrievedDocVal)
	if err != nil {
		return nil, docCas, err
	}

	valMap, ok := retrievedDocVal.(map[string]interface{})
	if !ok {
		err = fmt.Errorf("Retrieved document (%v) value is not a valid key-value map", docId)
		return nil, docCas, err
	}

	bodySlice, err := json.Marshal(valMap)
	if err != nil {
		return nil, docCas, err
	}

	return bodySlice, docCas, err
}

func (u *Utilities) FilterExpressionMatchesDoc(expression, docId, username, password, bucketName, addr string, port uint16) (result bool, err error) {
	var bodySlice []byte
	var docCas gocb.Cas

	cluster, err := gocb.Connect(fmt.Sprintf("http://%v:%v", addr, port))
	if err != nil {
		return
	}

	cluster.Authenticate(gocb.PasswordAuthenticator{
		Username: username,
		Password: password,
	})

	bucket, err := cluster.OpenBucket(bucketName, "")
	if err != nil {
		return
	}

	retrieveRetryOp := func() ([]byte, error) {
		bodySlice, docCas, err = filterExpressionGetDocVal(bucket, docId)
		if err != nil {
			err = fmt.Errorf("Error getting doc %v value: %v", docId, err.Error())
			return nil, err
		}

		if base.FilterContainsXattrExpression(expression) {
			xattrSlice, err := filterExpressionGetXattrHelper(bucket, docId, docCas)
			if err != nil {
				err = fmt.Errorf("Error getting doc %v xattributes: %v", docId, err.Error())
				return nil, err
			}

			bodySlice, err = base.AddXattrToBeFilteredWithoutDP(bodySlice, xattrSlice)
			if err != nil {
				err = fmt.Errorf("Error adding doc %v xattributes to be filtered: %v", docId, err.Error())
				return nil, err
			}
		}

		if base.FilterContainsKeyExpression(expression) {
			testBytes := []byte(docId)
			bodySlice, err, _ = base.AddKeyToBeFiltered(bodySlice, testBytes, nil, nil, len(testBytes)-1)
			if err != nil {
				err = fmt.Errorf("Error adding doc %v ID to be filtered: %v", docId, err.Error())
			}
		}
		return bodySlice, err
	}

	retryOp := func() error {
		bodySlice, err = retrieveRetryOp()
		return err
	}
	err = u.ExponentialBackoffExecutor("filterTesterXattrRetriever", base.BucketInfoOpWaitTime, base.BucketInfoOpMaxRetry, base.BucketInfoOpRetryFactor, retryOp)
	if err != nil {
		if strings.Contains(err.Error(), base.ErrorInvalidCAS.Error()) {
			err = fmt.Errorf("Unable to successfully retrieve document %v because it keeps mutating", docId)
		}
		return
	}

	matcher, err := gojsonsm.GetFilterExpressionMatcher(base.ReplaceKeyWordsForExpression(expression))
	if err != nil {
		err = fmt.Errorf("Error filtering doc %v ID: %v", docId, err.Error())
		return
	}

	result, err = matcher.Match(bodySlice)
	return
}

// given a matches map, convert the indices from byte index to rune index
func (u *Utilities) convertByteIndexToRuneIndex(key string, matches [][]int) ([][]int, error) {
	convertedMatches := make([][]int, 0)
	if len(key) == 0 || len(matches) == 0 {
		return matches, nil
	}

	// parse key and build a byte index to rune index map
	indexMap := make(map[int]int)
	byteIndex := 0
	runeIndex := 0
	keyBytes := []byte(key)
	keyLen := len(key)
	for {
		indexMap[byteIndex] = runeIndex
		if byteIndex < keyLen {
			_, runeLen := utf8.DecodeRune(keyBytes[byteIndex:])
			byteIndex += runeLen
			runeIndex++
		} else {
			break
		}
	}

	if u.logger_utils.GetLogLevel() >= log.LogLevelDebug {
		u.logger_utils.Debugf("key=%v, indexMap=%v%v%v\n", base.UdTagBegin, key, base.UdTagEnd, indexMap)
	}

	var ok bool
	for _, match := range matches {
		convertedMatch := make([]int, 2)
		convertedMatch[0], ok = indexMap[match[0]]
		if !ok {
			// should not happen
			errMsg := u.InvalidRuneIndexErrorMessage(key, match[0])
			u.logger_utils.Errorf(errMsg)
			return nil, errors.New(errMsg)
		}
		convertedMatch[1], ok = indexMap[match[1]]
		if !ok {
			// should not happen
			errMsg := u.InvalidRuneIndexErrorMessage(key, match[1])
			u.logger_utils.Errorf(errMsg)
			return nil, errors.New(errMsg)
		}
		convertedMatches = append(convertedMatches, convertedMatch)
	}

	return convertedMatches, nil
}

func (u *Utilities) InvalidRuneIndexErrorMessage(key string, index int) string {
	return fmt.Sprintf("byte index, %v, in match for key, %v, is not a starting index for a rune", index, key)
}

func (u *Utilities) LocalBucketUUID(local_connStr string, bucketName string, logger *log.CommonLogger) (string, error) {
	return u.BucketUUID(local_connStr, bucketName, "", "", base.HttpAuthMechPlain, nil, false, nil, nil, logger)
}

func (u *Utilities) LocalBucketPassword(local_connStr string, bucketName string, logger *log.CommonLogger) (string, error) {
	return u.BucketPassword(local_connStr, bucketName, "", "", base.HttpAuthMechPlain, nil, false, nil, nil, logger)
}

func (u *Utilities) ReplicationStatusNotFoundError(topic string) error {
	return fmt.Errorf("Cannot find replication status for topic %v", topic)
}

func (u *Utilities) BucketNotFoundError(bucketName string) error {
	return fmt.Errorf("Bucket `%v` not found.", bucketName)
}

// creates a local memcached connection.
// always use plain auth
func (u *Utilities) GetMemcachedConnectionWFeatures(serverAddr, bucketName, userAgent string,
	keepAlivePeriod time.Duration, features HELOFeatures, logger *log.CommonLogger) (mcc.ClientIface, HELOFeatures, error) {
	var respondedFeatures HELOFeatures

	logger.Infof("GetMemcachedConnection serverAddr=%v, bucketName=%v\n", serverAddr, bucketName)
	if serverAddr == "" {
		err := fmt.Errorf("Failed to get memcached connection because serverAddr is empty. bucketName=%v, userAgent=%v", bucketName, userAgent)
		logger.Warnf(err.Error())
		return nil, respondedFeatures, err
	}
	username, password, err := cbauth.GetMemcachedServiceAuth(serverAddr)
	if u.logger_utils.GetLogLevel() >= log.LogLevelDebug {
		logger.Debugf("memcached auth: username=%v%v%v, password=%v%v%v, err=%v\n", base.UdTagBegin, username, base.UdTagEnd, base.UdTagBegin, password, base.UdTagEnd, err)
	}
	if err != nil {
		return nil, respondedFeatures, err
	}

	return u.GetRemoteMemcachedConnectionWFeatures(serverAddr, username, password, bucketName, userAgent, true /*plainAuth*/, keepAlivePeriod, features, logger)
}

func (u *Utilities) GetMemcachedConnection(serverAddr, bucketName, userAgent string,
	keepAlivePeriod time.Duration, logger *log.CommonLogger) (mcc.ClientIface, error) {
	var noFeatures HELOFeatures
	clientIface, _, err := u.GetMemcachedConnectionWFeatures(serverAddr, bucketName, userAgent, keepAlivePeriod, noFeatures, logger)
	return clientIface, err
}

func (u *Utilities) GetMemcachedRawConn(serverAddr, username, password, bucketName string, plainAuth bool,
	keepAlivePeriod time.Duration, logger *log.CommonLogger) (mcc.ClientIface, error) {
	conn, err := base.NewConn(serverAddr, username, password, bucketName, plainAuth, keepAlivePeriod, logger)
	if err != nil {
		return nil, err
	}
	return conn, err
}

func (u *Utilities) GetRemoteMemcachedConnectionWFeatures(serverAddr, username, password, bucketName, userAgent string,
	plainAuth bool, keepAlivePeriod time.Duration, features HELOFeatures, logger *log.CommonLogger) (mcc.ClientIface, HELOFeatures, error) {
	var err error
	var conn mcc.ClientIface
	var respondedFeatures HELOFeatures

	getRemoteMcConnOp := func() error {
		conn, err = u.GetMemcachedRawConn(serverAddr, username, password, bucketName, plainAuth, keepAlivePeriod, logger)
		if err != nil {
			logger.Warnf("Failed to construct memcached client for %v, err=%v\n", serverAddr, err)
			return err
		}

		respondedFeatures, err = u.SendHELOWithFeatures(conn, userAgent, base.HELOTimeout, base.HELOTimeout, features, logger)

		if err != nil {
			conn.Close()
			logger.Warnf("Failed to send HELO for %v, err=%v\n", conn, err)
			return err
		}
		return nil
	}

	opErr := u.ExponentialBackoffExecutor("GetRemoteMemcachedConnection", base.RemoteMcRetryWaitTime, base.MaxRemoteMcRetry,
		base.RemoteMcRetryFactor, getRemoteMcConnOp)

	if opErr != nil {
		logger.Errorf(opErr.Error())
		return nil, respondedFeatures, err
	}

	return conn, respondedFeatures, err
}

func (u *Utilities) GetRemoteMemcachedConnection(serverAddr, username, password, bucketName, userAgent string,
	plainAuth bool, keepAlivePeriod time.Duration, logger *log.CommonLogger) (mcc.ClientIface, error) {
	var noFeatureEnabled HELOFeatures
	conn, _, err := u.GetRemoteMemcachedConnectionWFeatures(serverAddr, username, password, bucketName, userAgent, plainAuth, keepAlivePeriod,
		noFeatureEnabled, logger)
	return conn, err
}

// send helo with specified user agent string to memcached
// the helo is purely informational, for the identification of the client
// unsuccessful response is not treated as errors
func (u *Utilities) SendHELO(client mcc.ClientIface, userAgent string, readTimeout, writeTimeout time.Duration,
	logger *log.CommonLogger) (err error) {
	var allFeaturesDisabled HELOFeatures
	heloReq := u.ComposeHELORequest(userAgent, allFeaturesDisabled)

	var response *mc.MCResponse
	response, err = u.sendHELORequest(client, heloReq, userAgent, readTimeout, writeTimeout, logger)
	if err != nil {
		logger.Errorf("Received error response from HELO command. userAgent=%v, err=%v.", userAgent, err)
	} else if response.Status != mc.SUCCESS {
		logger.Warnf("Received unexpected response from HELO command. userAgent=%v, response status=%v.", userAgent, response.Status)
	} else {
		logger.Infof("Successfully sent HELO command with userAgent=%v", userAgent)
	}
	return
}

// send helo to memcached with data type (including xattr) feature enabled
// used exclusively by xmem nozzle
// we need to know whether data type is indeed enabled from helo response
// unsuccessful response is treated as errors
func (u *Utilities) SendHELOWithFeatures(client mcc.ClientIface, userAgent string, readTimeout, writeTimeout time.Duration, requestedFeatures HELOFeatures, logger *log.CommonLogger) (respondedFeatures HELOFeatures, err error) {
	// Initially set initial respondedFeatures to None since no compression negotiated should not be invalid
	respondedFeatures.CompressionType = base.CompressionTypeNone

	heloReq := u.ComposeHELORequest(userAgent, requestedFeatures)

	var response *mc.MCResponse
	response, err = u.sendHELORequest(client, heloReq, userAgent, readTimeout, writeTimeout, logger)
	if err != nil {
		logger.Errorf("Received error response from HELO command. userAgent=%v, err=%v.", userAgent, err)
	} else if response.Status != mc.SUCCESS {
		errMsg := fmt.Sprintf("Received unexpected response from HELO command. userAgent=%v, response status=%v.", userAgent, response.Status)
		logger.Error(errMsg)
		err = errors.New(errMsg)
	} else {
		// helo succeeded. parse response body for features enabled
		bodyLen := len(response.Body)
		if (bodyLen & 1) != 0 {
			// body has to have even number of bytes
			logger.Errorf("Received response body with odd number of bytes from HELO command. userAgent=%v, (redacted) response body=%v%v%v.", userAgent, base.UdTagBegin, response.Body, base.UdTagEnd)
			err = errors.New(fmt.Sprintf("Received response body with odd number of bytes from HELO command. userAgent=%v.", userAgent))
			return
		}
		pos := 0
		for {
			if pos >= bodyLen {
				break
			}
			feature := binary.BigEndian.Uint16(response.Body[pos : pos+2])
			if feature == base.HELO_FEATURE_XATTR {
				respondedFeatures.Xattribute = true
			}
			if feature == base.HELO_FEATURE_SNAPPY {
				respondedFeatures.CompressionType = base.CompressionTypeSnappy
			}
			pos += 2
		}
		logger.Infof("Successfully sent HELO command with userAgent=%v. attributes=%v", userAgent, respondedFeatures)
	}
	return
}

func (u *Utilities) sendHELORequest(client mcc.ClientIface, heloReq *mc.MCRequest, userAgent string, readTimeout, writeTimeout time.Duration,
	logger *log.CommonLogger) (response *mc.MCResponse, err error) {

	conn := client.Hijack()
	conn.(net.Conn).SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err = conn.Write(heloReq.Bytes())
	conn.(net.Conn).SetWriteDeadline(time.Time{})
	if err != nil {
		logger.Warnf("Error sending HELO command. userAgent=%v, err=%v.", userAgent, err)
		return
	}

	conn.(net.Conn).SetReadDeadline(time.Now().Add(readTimeout))
	response, err = client.Receive()
	conn.(net.Conn).SetReadDeadline(time.Time{})
	return
}

// compose a HELO command
func (u *Utilities) ComposeHELORequest(userAgent string, features HELOFeatures) *mc.MCRequest {
	var value []byte
	var numOfFeatures = features.NumberOfActivatedFeatures()
	var sliceIndex int
	bytesToAllocate := base.HELO_BYTES_PER_FEATURE * (numOfFeatures + 1) // TCP_NO_DELAY is included by default
	value = make([]byte, bytesToAllocate)

	// tcp no delay - [0:2]
	binary.BigEndian.PutUint16(value[sliceIndex:sliceIndex+base.HELO_BYTES_PER_FEATURE], base.HELO_FEATURE_TCP_NO_DELAY)
	sliceIndex += base.HELO_BYTES_PER_FEATURE

	// Xattribute
	if features.Xattribute {
		binary.BigEndian.PutUint16(value[sliceIndex:sliceIndex+base.HELO_BYTES_PER_FEATURE], base.HELO_FEATURE_XATTR)
		sliceIndex += base.HELO_BYTES_PER_FEATURE
	}

	// Compression
	if features.CompressionType == base.CompressionTypeSnappy {
		binary.BigEndian.PutUint16(value[sliceIndex:sliceIndex+base.HELO_BYTES_PER_FEATURE], base.HELO_FEATURE_SNAPPY)
		sliceIndex += base.HELO_BYTES_PER_FEATURE
	}

	return &mc.MCRequest{
		Key:    []byte(userAgent),
		Opcode: mc.HELLO,
		Body:   value,
	}
}

func (u *Utilities) GetIntSettingFromSettings(settings metadata.ReplicationSettingsMap, settingName string) (int, error) {
	settingObj := u.GetSettingFromSettings(settings, settingName)
	if settingObj == nil {
		return -1, nil
	}

	setting, ok := settingObj.(int)
	if !ok {
		return -1, fmt.Errorf("Setting %v is of wrong type", settingName)
	}

	return setting, nil
}

func (u *Utilities) GetStringSettingFromSettings(settings metadata.ReplicationSettingsMap, settingName string) (string, error) {
	settingObj := u.GetSettingFromSettings(settings, settingName)
	if settingObj == nil {
		return "", nil
	}

	setting, ok := settingObj.(string)
	if !ok {
		return "", fmt.Errorf("Setting %v is of wrong type", settingName)
	}

	return setting, nil
}

func (u *Utilities) GetSettingFromSettings(settings metadata.ReplicationSettingsMap, settingName string) interface{} {
	if settings == nil {
		return nil
	}

	setting, ok := settings[settingName]
	if !ok {
		return nil
	}

	return setting
}

func (u *Utilities) GetMemcachedClient(serverAddr, bucketName string, kv_mem_clients map[string]mcc.ClientIface,
	userAgent string, keepAlivePeriod time.Duration, logger *log.CommonLogger) (mcc.ClientIface, error) {
	client, ok := kv_mem_clients[serverAddr]
	if ok {
		return client, nil
	} else {
		if bucketName == "" {
			err := fmt.Errorf("Failed to get memcached client because of unexpected empty bucketName. serverAddr=%v, userAgent=%v", serverAddr, userAgent)
			logger.Warnf(err.Error())
			return nil, err
		}

		var client, err = u.GetMemcachedConnection(serverAddr, bucketName, userAgent, keepAlivePeriod, logger)
		if err == nil {
			kv_mem_clients[serverAddr] = client
			return client, nil
		} else {
			return nil, err
		}
	}
}

func (u *Utilities) GetServerVBucketsMap(connStr, bucketName string, bucketInfo map[string]interface{}) (map[string][]uint16, error) {
	vbucketServerMapObj, ok := bucketInfo[base.VBucketServerMapKey]
	if !ok {
		return nil, fmt.Errorf("Error getting vbucket server map from bucket info. connStr=%v, bucketName=%v, bucketInfo=%v\n", connStr, bucketName, bucketInfo)
	}
	vbucketServerMap, ok := vbucketServerMapObj.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("Vbucket server map is of wrong type. connStr=%v, bucketName=%v, vbucketServerMap=%v\n", connStr, bucketName, vbucketServerMapObj)
	}

	// get server list
	serverListObj, ok := vbucketServerMap[base.ServerListKey]
	if !ok {
		return nil, fmt.Errorf("Error getting server list from vbucket server map. connStr=%v, bucketName=%v, vbucketServerMap=%v\n", connStr, bucketName, vbucketServerMap)
	}
	serverList, ok := serverListObj.([]interface{})
	if !ok {
		return nil, fmt.Errorf("Server list is of wrong type. connStr=%v, bucketName=%v, serverList=%v\n", connStr, bucketName, serverListObj)
	}

	servers := make([]string, len(serverList))
	for index, serverName := range serverList {
		serverNameStr, ok := serverName.(string)
		if !ok {
			return nil, fmt.Errorf("Server name is of wrong type. connStr=%v, bucketName=%v, serverName=%v\n", connStr, bucketName, serverName)
		}
		servers[index] = serverNameStr
	}

	// get vbucket "map"
	vbucketMapObj, ok := vbucketServerMap[base.VBucketMapKey]
	if !ok {
		return nil, fmt.Errorf("Error getting vbucket map from vbucket server map. connStr=%v, bucketName=%v, vbucketServerMap=%v\n", connStr, bucketName, vbucketServerMap)
	}
	vbucketMap, ok := vbucketMapObj.([]interface{})
	if !ok {
		return nil, fmt.Errorf("Vbucket map is of wrong type. connStr=%v, bucketName=%v, vbucketMap=%v\n", connStr, bucketName, vbucketMapObj)
	}

	serverVBMap := make(map[string][]uint16)

	for vbno, indexListObj := range vbucketMap {
		indexList, ok := indexListObj.([]interface{})
		if !ok {
			return nil, fmt.Errorf("Index list is of wrong type. connStr=%v, bucketName=%v, indexList=%v\n", connStr, bucketName, indexListObj)
		}
		if len(indexList) == 0 {
			return nil, fmt.Errorf("Index list is empty. connStr=%v, bucketName=%v, vbno=%v\n", connStr, bucketName, vbno)
		}
		indexFloat, ok := indexList[0].(float64)
		if !ok {
			return nil, fmt.Errorf("Master index is of wrong type. connStr=%v, bucketName=%v, index=%v\n", connStr, bucketName, indexList[0])
		}
		indexInt := int(indexFloat)
		if indexInt >= len(servers) {
			return nil, fmt.Errorf("Master index is out of range. connStr=%v, bucketName=%v, index=%v\n", connStr, bucketName, indexInt)
		} else if indexInt < 0 {
			// During rebalancing or topology changes, it's possible ns_server may return a -1 for index. Callers should treat it as an transient error.
			return nil, fmt.Errorf(fmt.Sprintf("%v connStr=%v, bucketName=%v, index=%v\n", base.ErrorMasterNegativeIndex, connStr, bucketName, indexInt))
		}

		server := servers[indexInt]
		var vbList []uint16
		vbList, ok = serverVBMap[server]
		if !ok {
			vbList = make([]uint16, 0)
		}
		vbList = append(vbList, uint16(vbno))
		serverVBMap[server] = vbList
	}
	return serverVBMap, nil
}

func (u *Utilities) GetRemoteServerVBucketsMap(connStr, bucketName string, bucketInfo map[string]interface{}) (kvVbMap map[string][]uint16, err error) {
	kvVbMap, err = u.GetServerVBucketsMap(connStr, bucketName, bucketInfo)
	if err != nil {
		return
	}
	u.TranslateKvVbMap(kvVbMap, bucketInfo)
	return
}

// get bucket type setting from bucket info
func (u *Utilities) GetBucketTypeFromBucketInfo(bucketName string, bucketInfo map[string]interface{}) (string, error) {
	bucketType := ""
	bucketTypeObj, ok := bucketInfo[base.BucketTypeKey]
	if !ok {
		return "", fmt.Errorf("Error looking up bucket type of bucket %v", bucketName)
	} else {
		bucketType, ok = bucketTypeObj.(string)
		if !ok {
			return "", fmt.Errorf("bucketType on bucket %v is of wrong type.", bucketName)
		}
	}
	return bucketType, nil
}

// check if a bucket belongs to an elastic search (es) cluster by looking for "authType" field in bucket info.
// if not found, cluster is es
func (u *Utilities) CheckWhetherClusterIsESBasedOnBucketInfo(bucketInfo map[string]interface{}) bool {
	_, ok := bucketInfo[base.AuthTypeKey]
	return !ok
}

// get conflict resolution type setting from bucket info
func (u *Utilities) GetConflictResolutionTypeFromBucketInfo(bucketName string, bucketInfo map[string]interface{}) (string, error) {
	conflictResolutionType := base.ConflictResolutionType_Seqno
	conflictResolutionTypeObj, ok := bucketInfo[base.ConflictResolutionTypeKey]
	if ok {
		conflictResolutionType, ok = conflictResolutionTypeObj.(string)
		if !ok {
			return "", fmt.Errorf("ConflictResolutionType on bucket %v is of wrong type.", bucketName)
		}
	}
	return conflictResolutionType, nil
}

// get EvictionPolicy setting from bucket info
func (u *Utilities) GetEvictionPolicyFromBucketInfo(bucketName string, bucketInfo map[string]interface{}) (string, error) {
	evictionPolicy := ""
	evictionPolicyObj, ok := bucketInfo[base.EvictionPolicyKey]
	if ok {
		evictionPolicy, ok = evictionPolicyObj.(string)
		if !ok {
			return "", fmt.Errorf("EvictionPolicy on bucket %v is of wrong type.", bucketName)
		}
	}
	return evictionPolicy, nil
}

/**
 * The second section is couchbase REST related utility functions
 */
// This method is used to get the SSL port for target nodes - will use alternate fields if possible
func (u *Utilities) GetMemcachedSSLPortMap(connStr, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool,
	clientCertificate []byte, clientKey []byte, bucket string, logger *log.CommonLogger) (base.SSLPortMap, error) {
	ret := make(base.SSLPortMap)

	logger.Infof("GetMemcachedSSLPort, connStr=%v\n", connStr)
	bucketInfo, err := u.GetClusterInfo(connStr, base.BPath+bucket, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return nil, err
	}

	nodesExt, ok := bucketInfo[base.NodeExtKey]
	if !ok {
		return nil, u.BucketInfoParseError(bucketInfo, logger)
	}

	nodesExtArray, ok := nodesExt.([]interface{})
	if !ok {
		return nil, u.BucketInfoParseError(bucketInfo, logger)
	}

	var hostName string
	for _, nodeExt := range nodesExtArray {
		var portNumberToUse uint16
		nodeExtMap, ok := nodeExt.(map[string]interface{})
		if !ok {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}

		// note that this is the only place where nodeExtMap contains a hostname without port
		// instead of a host address with port
		hostName, err = u.getHostNameWithoutPortFromNodeInfo(connStr, nodeExtMap, logger)
		if err != nil {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}

		// Internal key
		service, ok := nodeExtMap[base.ServicesKey]
		if !ok {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}

		services_map, ok := service.(map[string]interface{})
		if !ok {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}

		kv_port, ok := services_map[base.KVPortKey]
		if !ok {
			// the node may not have kv services. skip the node
			continue
		}
		kvPortFloat, ok := kv_port.(float64)
		if !ok {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}

		hostAddr := base.GetHostAddr(hostName, uint16(kvPortFloat))

		kv_ssl_port, ok := services_map[base.KVSSLPortKey]
		if !ok {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}

		kvSSLPortFloat, ok := kv_ssl_port.(float64)
		if !ok {
			return nil, u.BucketInfoParseError(bucketInfo, logger)
		}
		portNumberToUse = uint16(kvSSLPortFloat)

		// Since this is a call intended for targets, get the external info
		externalHostAddr, externalKVPort, externalKVPortErr, externalSSLPort, externalSSLPortErr := u.GetExternalAddressAndKvPortsFromNodeInfo(nodeExtMap)
		if len(externalHostAddr) > 0 {
			if externalKVPortErr == nil {
				// External address and port both exist
				hostAddr = base.GetHostAddr(externalHostAddr, uint16(externalKVPort))
			} else if externalKVPortErr == base.ErrorNoPortNumber {
				// External address exists, but port does not. Use internal host's port number
				hostAddr = base.GetHostAddr(externalHostAddr, uint16(kvPortFloat))
			}
		}
		if externalSSLPortErr == nil {
			portNumberToUse = uint16(externalSSLPort)
		}

		ret[hostAddr] = portNumberToUse
	}
	logger.Infof("memcached ssl port map=%v\n", ret)

	return ret, nil
}

func (u *Utilities) BucketInfoParseError(bucketInfo map[string]interface{}, logger *log.CommonLogger) error {
	errMsg := "Error parsing memcached ssl port of remote cluster."
	detailedErrMsg := errMsg + fmt.Sprintf("bucketInfo=%v", bucketInfo)
	logger.Errorf(detailedErrMsg)
	return fmt.Errorf(errMsg)
}

func (u *Utilities) HttpsRemoteHostAddr(hostAddr string, logger *log.CommonLogger) (string, error) {
	// Extract hostname to be combined with SSL port
	hostName := base.GetHostName(hostAddr)
	// Extract SSL port, prioritizing externalAddress SSL port if it is there
	sslPort, err := u.GetRemoteSSLPort(hostAddr, logger)
	if err != nil {
		return "", err
	}
	return base.GetHostAddr(hostName, sslPort), nil
}

func (u *Utilities) GetRemoteSSLPort(hostAddr string, logger *log.CommonLogger) (uint16, error) {
	var portNumber uint16
	portInfo := make(map[string]interface{})
	err, statusCode := u.QueryRestApiWithAuth(hostAddr, base.SSLPortsPath, false, "", "", base.HttpAuthMechPlain, nil, false, nil, nil, base.MethodGet, "", nil, 0, &portInfo, nil, false, logger)
	if err == nil && statusCode == http.StatusUnauthorized {
		// SSLPorts request normally do not require any user credentials
		// the only place unauthorized error could be returned is when target is elasticsearch cluster
		// treat this case differently so that a more specific error message can be returned to user
		return 0, base.ErrorUnauthorized
	}
	if err != nil || statusCode != http.StatusOK {
		return 0, fmt.Errorf("Failed on calling %v, err=%v, statusCode=%v", base.SSLPortsPath, err, statusCode)
	}

	// If the external exists, use that, otherwise use the internal SSL port
	externalSSLPort, externalErr := u.getExternalSSLMgtPort(portInfo)
	if externalErr == nil {
		portNumber = (uint16)(externalSSLPort)
	} else {
		sslPort, ok := portInfo[base.SSLPortKey]
		if !ok {
			errMsg := "Failed to parse port info. ssl port is missing."
			logger.Errorf("%v. portInfo=%v", errMsg, portInfo)
			return 0, fmt.Errorf(errMsg)
		}

		sslPortFloat, ok := sslPort.(float64)
		if !ok {
			return 0, fmt.Errorf("ssl port is of wrong type. Expected type: float64; Actual type: %s", reflect.TypeOf(sslPort))
		}
		portNumber = uint16(sslPortFloat)
	}

	return portNumber, nil
}

func (u *Utilities) GetClusterInfoWStatusCode(hostAddr, path, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (map[string]interface{}, error, int) {
	clusterInfo := make(map[string]interface{})
	err, statusCode := u.QueryRestApiWithAuth(hostAddr, path, false, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, base.MethodGet, "", nil, 0, &clusterInfo, nil, false, logger)
	if err != nil || statusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed on calling host=%v, path=%v, err=%v, statusCode=%v", hostAddr, path, err, statusCode), statusCode
	}
	return clusterInfo, nil, statusCode
}

func (u *Utilities) GetClusterInfo(hostAddr, path, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (map[string]interface{}, error) {
	clusterInfo, err, _ := u.GetClusterInfoWStatusCode(hostAddr, path, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	return clusterInfo, err
}

func (u *Utilities) GetClusterUUID(hostAddr, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (string, error) {
	clusterInfo, err := u.GetClusterInfo(hostAddr, base.PoolsPath, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return "", err
	}
	clusterUUIDObj, ok := clusterInfo[base.UUIDKey]
	if !ok {
		return "", fmt.Errorf("Cannot find uuid key in cluster info. hostAddr=%v, clusterInfo=%v\n", hostAddr, clusterInfo)
	}
	clusterUUID, ok := clusterUUIDObj.(string)
	if !ok {
		// cluster uuid is "[]" for unintialized cluster
		_, ok = clusterUUIDObj.([]interface{})
		if ok {
			return "", fmt.Errorf("cluster %v is not initialized. clusterUUIDObj=%v\n", hostAddr, clusterUUIDObj)
		} else {
			return "", fmt.Errorf("uuid key in cluster info is not of string type. hostAddr=%v, clusterUUIDObj=%v\n", hostAddr, clusterUUIDObj)
		}
	}
	return clusterUUID, nil
}

// get a list of node infos with full info
// this api calls xxx/pools/nodes, which returns full node info including clustercompatibility, etc.
// the catch is that this xxx/pools/nodes is not supported by elastic search cluster
func (u *Utilities) GetNodeListWithFullInfo(hostAddr, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) ([]interface{}, error) {
	clusterInfo, err := u.GetClusterInfo(hostAddr, base.NodesPath, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return nil, err
	}

	return u.GetNodeListFromInfoMap(clusterInfo, logger)

}

// get a list of node infos with minimum info
// this api calls xxx/pools/default, which returns a subset of node info such as hostname
// this api can/needs to be used when connecting to elastic search cluster, which supports xxx/pools/default
func (u *Utilities) GetNodeListWithMinInfo(hostAddr, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) ([]interface{}, error) {
	clusterInfo, err := u.GetClusterInfo(hostAddr, base.DefaultPoolPath, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return nil, err
	}

	return u.GetNodeListFromInfoMap(clusterInfo, logger)

}

func (u *Utilities) GetClusterUUIDAndNodeListWithMinInfo(hostAddr, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (string, []interface{}, error) {
	defaultPoolInfo, err := u.GetClusterInfo(hostAddr, base.DefaultPoolPath, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return "", nil, err
	}

	return u.GetClusterUUIDAndNodeListWithMinInfoFromDefaultPoolInfo(defaultPoolInfo, logger)

}

func (u *Utilities) GetClusterUUIDAndNodeListWithMinInfoFromDefaultPoolInfo(defaultPoolInfo map[string]interface{}, logger *log.CommonLogger) (string, []interface{}, error) {
	clusterUUID, err := u.GetClusterUUIDFromDefaultPoolInfo(defaultPoolInfo, logger)
	if err != nil {
		return "", nil, err
	}

	nodeList, err := u.GetNodeListFromInfoMap(defaultPoolInfo, logger)

	return clusterUUID, nodeList, err

}

// get bucket info
// a specialized case of GetClusterInfo
func (u *Utilities) GetBucketInfo(hostAddr, bucketName, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (map[string]interface{}, error) {
	bucketInfo := make(map[string]interface{})
	err, statusCode := u.QueryRestApiWithAuth(hostAddr, base.DefaultPoolBucketsPath+bucketName, false, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, base.MethodGet, "", nil, 0, &bucketInfo, nil, false, logger)
	if err == nil && statusCode == http.StatusOK {
		return bucketInfo, nil
	}
	if statusCode == http.StatusNotFound {
		return nil, u.GetNonExistentBucketError()
	} else {
		logger.Errorf("Failed to get bucket info for bucket '%v'. host=%v, err=%v, statusCode=%v", bucketName, hostAddr, err, statusCode)
		return nil, fmt.Errorf("Failed to get bucket info.")
	}
}

// get bucket uuid
func (u *Utilities) BucketUUID(hostAddr, bucketName, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (string, error) {
	bucketInfo, err := u.GetBucketInfo(hostAddr, bucketName, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return "", err
	}

	return u.GetBucketUuidFromBucketInfo(bucketName, bucketInfo, logger)
}

// get bucket password
func (u *Utilities) BucketPassword(hostAddr, bucketName, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (string, error) {
	bucketInfo, err := u.GetBucketInfo(hostAddr, bucketName, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
	if err != nil {
		return "", err
	}

	return u.GetBucketPasswordFromBucketInfo(bucketName, bucketInfo, logger)
}

func (u *Utilities) GetLocalBuckets(hostAddr string, logger *log.CommonLogger) (map[string]string, error) {
	return u.GetBuckets(hostAddr, "", "", base.HttpAuthMechPlain, nil, false, nil, nil, logger)
}

// return a map of buckets
// key = bucketName, value = bucketUUID
func (u *Utilities) GetBuckets(hostAddr, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte, logger *log.CommonLogger) (map[string]string, error) {
	bucketListInfo := make([]interface{}, 0)
	err, statusCode := u.QueryRestApiWithAuth(hostAddr, base.DefaultPoolBucketsPath, false, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, base.MethodGet, "", nil, 0, &bucketListInfo, nil, false, logger)
	if err != nil || statusCode != http.StatusOK {
		return nil, fmt.Errorf("Failed on calling host=%v, path=%v, err=%v, statusCode=%v", hostAddr, base.DefaultPoolBucketsPath, err, statusCode)
	}

	return u.GetBucketsFromInfoMap(bucketListInfo, logger)
}

func (u *Utilities) GetBucketsFromInfoMap(bucketListInfo []interface{}, logger *log.CommonLogger) (map[string]string, error) {
	buckets := make(map[string]string)
	for _, bucketInfo := range bucketListInfo {
		bucketInfoMap, ok := bucketInfo.(map[string]interface{})
		if !ok {
			errMsg := fmt.Sprintf("bucket info is not of map type.  bucket info=%v", bucketInfo)
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}
		bucketNameInfo, ok := bucketInfoMap[base.BucketNameKey]
		if !ok {
			errMsg := fmt.Sprintf("bucket info does not contain bucket name.  bucket info=%v", bucketInfoMap)
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}
		bucketName, ok := bucketNameInfo.(string)
		if !ok {
			errMsg := fmt.Sprintf("bucket name is not of string type.  bucket name=%v", bucketNameInfo)
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}
		bucketUUIDInfo, ok := bucketInfoMap[base.UUIDKey]
		if !ok {
			errMsg := fmt.Sprintf("bucket info does not contain bucket uuid.  bucket info=%v", bucketInfoMap)
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}
		bucketUUID, ok := bucketUUIDInfo.(string)
		if !ok {
			errMsg := fmt.Sprintf("bucket uuid is not of string type.  bucket uuid=%v", bucketUUIDInfo)
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}
		buckets[bucketName] = bucketUUID
	}

	return buckets, nil
}

func (u *Utilities) BucketValidationInfo(hostAddr, bucketName, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte,
	logger *log.CommonLogger) (bucketInfo map[string]interface{}, bucketType string, bucketUUID string, bucketConflictResolutionType string,
	bucketEvictionPolicy string, bucketKVVBMap map[string][]uint16, err error) {

	return u.bucketValidationInfoInternal(hostAddr, bucketName, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger, false /*external*/)
}

func (u *Utilities) RemoteBucketValidationInfo(hostAddr, bucketName, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte,
	logger *log.CommonLogger) (bucketInfo map[string]interface{}, bucketType string, bucketUUID string, bucketConflictResolutionType string,
	bucketEvictionPolicy string, bucketKVVBMap map[string][]uint16, err error) {

	return u.bucketValidationInfoInternal(hostAddr, bucketName, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger, true /*external*/)
}

// get a number of fields in bucket for validation purpose
// 1. bucket type
// 2. bucket uuid
// 3. bucket conflict resolution type
// 4. bucket eviction policy
// 5. bucket server vb map
func (u *Utilities) bucketValidationInfoInternal(hostAddr, bucketName, username, password string, authMech base.HttpAuthMech, certificate []byte, sanInCertificate bool, clientCertificate, clientKey []byte,
	logger *log.CommonLogger, remote bool) (bucketInfo map[string]interface{}, bucketType string, bucketUUID string, bucketConflictResolutionType string,
	bucketEvictionPolicy string, bucketKVVBMap map[string][]uint16, err error) {

	bucketValidationInfoOp := func() error {
		bucketInfo, err = u.GetBucketInfo(hostAddr, bucketName, username, password, authMech, certificate, sanInCertificate, clientCertificate, clientKey, logger)
		if err != nil {
			return err
		}

		bucketType, err = u.GetBucketTypeFromBucketInfo(bucketName, bucketInfo)
		if err != nil {
			err = fmt.Errorf("Error retrieving BucketType setting on bucket %v. err=%v", bucketName, err)
			return err
		}
		bucketUUID, err = u.GetBucketUuidFromBucketInfo(bucketName, bucketInfo, logger)
		if err != nil {
			err = fmt.Errorf("Error retrieving UUID setting on bucket %v. err=%v", bucketName, err)
			return err
		}
		bucketConflictResolutionType, err = u.GetConflictResolutionTypeFromBucketInfo(bucketName, bucketInfo)
		if err != nil {
			err = fmt.Errorf("Error retrieving ConflictResolutionType setting on bucket %v. err=%v", bucketName, err)
			return err
		}
		bucketEvictionPolicy, err = u.GetEvictionPolicyFromBucketInfo(bucketName, bucketInfo)
		if err != nil {
			err = fmt.Errorf("Error retrieving EvictionPolicy setting on bucket %v. err=%v", bucketName, err)
			return err
		}
		bucketKVVBMap, err = u.GetServerVBucketsMap(hostAddr, bucketName, bucketInfo)
		if err != nil {
			err = fmt.Errorf("Error retrieving server vb map on bucket %v. err=%v", bucketName, err)
			return err
		}

		if remote {
			u.TranslateKvVbMap(bucketKVVBMap, bucketInfo)
		}
		return nil
	}

	err = u.ExponentialBackoffExecutor("BucketValidationInfo", base.BucketInfoOpWaitTime, base.BucketInfoOpMaxRetry, base.BucketInfoOpRetryFactor, bucketValidationInfoOp)
	return
}

func (u *Utilities) GetBucketUuidFromBucketInfo(bucketName string, bucketInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	bucketUUID := ""
	bucketUUIDObj, ok := bucketInfo[base.UUIDKey]
	if !ok {
		return "", fmt.Errorf("Error looking up uuid of bucket %v", bucketName)
	} else {
		bucketUUID, ok = bucketUUIDObj.(string)
		if !ok {
			return "", fmt.Errorf("Uuid of bucket %v is of wrong type", bucketName)
		}
	}
	return bucketUUID, nil
}

func (u *Utilities) GetClusterUUIDFromDefaultPoolInfo(defaultPoolInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	bucketsObj, ok := defaultPoolInfo[base.BucketsKey]
	if !ok {
		errMsg := fmt.Sprintf("Cannot find buckets key in default pool info. defaultPoolInfo=%v\n", defaultPoolInfo)
		logger.Error(errMsg)
		return "", errors.New(errMsg)
	}
	bucketsInfo, ok := bucketsObj.(map[string]interface{})
	if !ok {
		errMsg := fmt.Sprintf("buckets in default pool info is not of map type. buckets=%v\n", bucketsObj)
		logger.Error(errMsg)
		return "", errors.New(errMsg)
	}
	uriObj, ok := bucketsInfo[base.URIKey]
	if !ok {
		errMsg := fmt.Sprintf("Cannot find uri key in buckets info. bucketsInfo=%v\n", bucketsInfo)
		logger.Error(errMsg)
		return "", errors.New(errMsg)
	}
	uri, ok := uriObj.(string)
	if !ok {
		errMsg := fmt.Sprintf("uri in buckets info is not of string type. uri=%v\n", uriObj)
		logger.Error(errMsg)
		return "", errors.New(errMsg)
	}

	return u.GetClusterUUIDFromURI(uri)
}

func (u *Utilities) GetClusterUUIDFromURI(uri string) (string, error) {
	// uri is in the form of /pools/default/buckets?uuid=d5dea23aa7ee3771becb3fcdb46ff956
	searchKey := base.UUIDKey + "="
	index := strings.LastIndex(uri, searchKey)
	if index < 0 {
		return "", fmt.Errorf("uri does not contain uuid. uri=%v", uri)
	}
	return uri[index+len(searchKey):], nil
}

func (u *Utilities) GetClusterCompatibilityFromBucketInfo(bucketInfo map[string]interface{}, logger *log.CommonLogger) (int, error) {
	nodeList, err := u.GetNodeListFromInfoMap(bucketInfo, logger)
	if err != nil {
		return 0, err
	}

	clusterCompatibility, err := u.GetClusterCompatibilityFromNodeList(nodeList)
	if err != nil {
		logger.Error(err.Error())
		return 0, err
	}

	return clusterCompatibility, nil
}

func (u *Utilities) GetBucketPasswordFromBucketInfo(bucketName string, bucketInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	bucketPassword := ""
	bucketPasswordObj, ok := bucketInfo[base.SASLPasswordKey]
	if !ok {
		return "", fmt.Errorf("Error looking up password of bucket %v", bucketName)
	} else {
		bucketPassword, ok = bucketPasswordObj.(string)
		if !ok {
			return "", fmt.Errorf("Password of bucket %v is of wrong type", bucketName)
		}
	}
	return bucketPassword, nil
}

func (u *Utilities) GetNodeListFromInfoMap(infoMap map[string]interface{}, logger *log.CommonLogger) ([]interface{}, error) {
	// get node list from the map
	nodes, ok := infoMap[base.NodesKey]
	if !ok {
		errMsg := fmt.Sprintf("info map contains no nodes. info map=%v", infoMap)
		logger.Error(errMsg)
		return nil, errors.New(errMsg)
	}

	nodeList, ok := nodes.([]interface{})
	if !ok {
		errMsg := fmt.Sprintf("nodes is not of list type. type of nodes=%v", reflect.TypeOf(nodes))
		logger.Error(errMsg)
		return nil, errors.New(errMsg)
	}

	// only return the nodes that are active
	activeNodeList := make([]interface{}, 0)
	for _, node := range nodeList {
		nodeInfoMap, ok := node.(map[string]interface{})
		if !ok {
			errMsg := fmt.Sprintf("node info is not of map type. type=%v", reflect.TypeOf(node))
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}
		clusterMembershipObj, ok := nodeInfoMap[base.ClusterMembershipKey]
		if !ok {
			// this could happen when target is elastic search cluster (or maybe very old couchbase cluster?)
			// consider the node to be "active" to be safe
			errMsg := fmt.Sprintf("node info map does not contain cluster membership. node info map=%v ", nodeInfoMap)
			logger.Debug(errMsg)
			activeNodeList = append(activeNodeList, node)
			continue
		}
		clusterMembership, ok := clusterMembershipObj.(string)
		if !ok {
			// play safe and return the node as active
			errMsg := fmt.Sprintf("cluster membership is not string type. type=%v ", reflect.TypeOf(clusterMembershipObj))
			logger.Warn(errMsg)
			activeNodeList = append(activeNodeList, node)
			continue
		}
		if clusterMembership == "" || clusterMembership == base.ClusterMembership_Active {
			activeNodeList = append(activeNodeList, node)
		}
	}

	return activeNodeList, nil
}

func (u *Utilities) GetClusterCompatibilityFromNodeList(nodeList []interface{}) (int, error) {
	if len(nodeList) > 0 {
		firstNode, ok := nodeList[0].(map[string]interface{})
		if !ok {
			return 0, fmt.Errorf("node info is of wrong type. node info=%v", nodeList[0])
		}
		clusterCompatibility, ok := firstNode[base.ClusterCompatibilityKey]
		if !ok {
			return 0, fmt.Errorf("Can't get cluster compatibility info. node info=%v\n If replicating to ElasticSearch node, use XDCR v1.", nodeList[0])
		}
		clusterCompatibilityFloat, ok := clusterCompatibility.(float64)
		if !ok {
			return 0, fmt.Errorf("cluster compatibility is not of int type. type=%v", reflect.TypeOf(clusterCompatibility))
		}
		return int(clusterCompatibilityFloat), nil
	}

	return 0, fmt.Errorf("node list is empty")
}

// Used externally only - returns a list of nodes for management access
// if needHttps is true, returns both http addresses and https addresses
// if needHttps is false, returns http addresses and empty https addresses
func (u *Utilities) GetRemoteNodeAddressesListFromNodeList(nodeList []interface{}, connStr string, needHttps bool, logger *log.CommonLogger) (base.StringPairList, error) {
	nodeAddressesList := make(base.StringPairList, len(nodeList))
	var hostAddr string
	var hostHttpsAddr string
	var err error
	index := 0

	for _, node := range nodeList {
		nodeInfoMap, ok := node.(map[string]interface{})
		if !ok {
			errMsg := fmt.Sprintf("node info is not of map type. type of node info=%v", reflect.TypeOf(node))
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}

		hostAddr, err = u.GetExternalHostAddrFromNodeInfo(connStr, nodeInfoMap, false /*isHttps*/, logger)
		if err != nil {
			errMsg := fmt.Sprintf("cannot get hostname from node info %v", nodeInfoMap)
			logger.Error(errMsg)
			return nil, errors.New(errMsg)
		}

		if needHttps {
			hostHttpsAddr, err = u.GetExternalHostAddrFromNodeInfo(connStr, nodeInfoMap, true /*isHttps*/, logger)
			if err != nil {
				errMsg := fmt.Sprintf("cannot get https hostname from node info %v", nodeInfoMap)
				logger.Error(errMsg)
				return nil, errors.New(errMsg)
			}
		} else {
			hostHttpsAddr = ""
		}

		nodeAddressesList[index] = base.StringPair{hostAddr, hostHttpsAddr}
		index++
	}
	return nodeAddressesList, nil
}

func (u *Utilities) GetHttpsMgtPortFromNodeInfo(nodeInfo map[string]interface{}) (int, error) {
	portsObjRaw, portsObjExists := nodeInfo[base.PortsKey]
	if !portsObjExists {
		return -1, base.ErrorNoPortNumber
	}

	portsObj, ok := portsObjRaw.(map[string]interface{})
	if !ok {
		return -1, base.ErrorNoPortNumber
	}

	sslPort, ok := portsObj[base.SSLPortKey]
	if !ok {
		return -1, base.ErrorNoPortNumber
	}

	sslPortFloat, ok := sslPort.(float64)
	if !ok {
		return -1, base.ErrorNoPortNumber
	}
	return int(sslPortFloat), nil
}

func (u *Utilities) GetExternalHostAddrFromNodeInfo(connStr string, nodeInfo map[string]interface{}, isHttps bool, logger *log.CommonLogger) (string, error) {
	// Internal node information
	hostAddr, err := u.GetHostAddrFromNodeInfo(connStr, nodeInfo, logger)
	if err != nil {
		errMsg := fmt.Sprintf("cannot get hostname from node info %v", nodeInfo)
		logger.Error(errMsg)
		return "", errors.New(errMsg)
	}

	if isHttps {
		sslPort, err := u.GetHttpsMgtPortFromNodeInfo(nodeInfo)
		if err != nil {
			return "", err
		}
		hostName := base.GetHostName(hostAddr)
		hostAddr = base.GetHostAddr(hostName, uint16(sslPort))
	}

	// If external info exists, replace accordingly - hostAddr is currently pointing to internalNode's info
	if externalAddr, externalMgtPort, externalErr := u.getExternalMgtHostAndPort(nodeInfo, isHttps); externalErr == nil {
		hostAddr = base.GetHostAddr(externalAddr, (uint16)(externalMgtPort))
	} else if externalErr == base.ErrorNoPortNumber {
		// Extract original internal node management port from above
		hostPort, portErr := base.GetPortNumber(hostAddr)
		if portErr == nil {
			// Combine externalHost:internalPort
			hostAddr = base.GetHostAddr(externalAddr, (uint16)(hostPort))
		} else {
			// Original internal address did not have port number, so continue to just have externalAddr[:noPort]
			hostAddr = externalAddr
		}
	}

	return hostAddr, nil
}

// Returns:
// 1. External IP
// 2. External kv port (if applicable, -1 if not found)
// 3. Returns nil if port exists - ErrorNoPortNumber if kv (direct) port doesn't exist
// 4. External KvSSL port (if applicable, -1 if not found)
// 5. Returns nil if SSL port exists - ErrorNoPortNumber if SSL port doesn't exist
// Any other errors are considered bad op
func (u *Utilities) GetExternalAddressAndKvPortsFromNodeInfo(nodeInfo map[string]interface{}) (string, int, error, int, error) {
	var hostAddr string
	var portNumber int
	var sslPortNumber int
	var portErr error
	var sslPortErr error

	alternateObjRaw, alternateExists := nodeInfo[base.AlternateKey]
	if !alternateExists {
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	alternateObj, ok := (alternateObjRaw).(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("GetExternalAddressAndKvPortsFromNodeInfo: Unable to convert alternateObj to map[string]interface{}")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	externalObjRaw, externalExists := alternateObj[base.ExternalKey]
	if !externalExists {
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	externalObj, ok := (externalObjRaw).(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("GetExternalAddressAndKvPortsFromNodeInfo: Unable to convert externalObj to map[string]interface{}")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	hostAddrObjRaw, hostAddrObjExists := externalObj[base.HostNameKey]
	if !hostAddrObjExists {
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	hostAddr, ok = (hostAddrObjRaw).(string)
	if !ok {
		u.logger_utils.Errorf("GetExternalAddressAndKvPortsFromNodeInfo: Unable to convert hostAddrObj to string")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	} else if len(hostAddr) == 0 {
		u.logger_utils.Errorf("GetExternalAddressAndKvPortsFromNodeInfo: Empty Hostname")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	portsObjRaw, portsObjExists := externalObj[base.PortsKey]
	if !portsObjExists {
		return hostAddr, -1, base.ErrorNoPortNumber, -1, base.ErrorNoPortNumber
	}

	portErr = base.ErrorNoPortNumber
	sslPortErr = base.ErrorNoPortNumber
	portNumber = -1
	sslPortNumber = -1
	portsObj, ok := portsObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Warnf("Unable to convert portsObj to map[string]interface{}")
	} else {
		// Get the External kv port (internally "direct") port if it's there
		// KV team wants clients to use nodeServices, which means that "direct" is used as an internal naming convention
		// The alternate address fields use "kv" as what "direct" means to traditional XDCR
		kvPortFloat, kvPortExists := portsObj[base.KVPortKey]
		if kvPortExists {
			kvPortIntCheck, ok := kvPortFloat.(float64)
			if ok {
				portNumber = (int)(kvPortIntCheck)
				portErr = nil
			}
		}
		// Get the SSL port if it is there
		sslPort, sslPortExists := portsObj[base.KVSSLPortKey]
		if sslPortExists {
			sslPortIntCheck, ok := sslPort.(float64)
			if ok {
				sslPortNumber = (int)(sslPortIntCheck)
				sslPortErr = nil
			}
		}
	}
	return hostAddr, portNumber, portErr, sslPortNumber, sslPortErr
}

func (u *Utilities) getExternalMgtHostAndPort(nodeInfo map[string]interface{}, isHttps bool) (string, int, error) {
	var hostAddr string
	var portErr error = base.ErrorNoPortNumber
	var portNumber int = -1
	alternateObjRaw, alternateExists := nodeInfo[base.AlternateKey]
	if !alternateExists {
		return "", -1, base.ErrorResourceDoesNotExist
	}

	alternateObj, ok := alternateObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalMgtHostAndPort: unable to cast alternateObj to map[string]interface{}")
		return "", -1, base.ErrorResourceDoesNotExist
	}

	externalObjRaw, externalExists := alternateObj[base.ExternalKey]
	if !externalExists {
		return "", -1, base.ErrorResourceDoesNotExist
	}

	externalObj, ok := externalObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalMgtHostAndPort: unable to cast externalObj to map[string]interface{}")
		return "", -1, base.ErrorResourceDoesNotExist
	}

	hostAddrObj, hostAddrObjExists := externalObj[base.HostNameKey]
	if !hostAddrObjExists {
		return "", -1, base.ErrorResourceDoesNotExist
	}

	hostAddr, ok = hostAddrObj.(string)
	if !ok {
		u.logger_utils.Errorf("getExternalMgtHostAndPort: unable to cast hostAddr to string")
		return "", -1, base.ErrorResourceDoesNotExist
	} else if len(hostAddr) == 0 {
		u.logger_utils.Errorf("getExternalMgtHostAndPort: empty hostAddr")
		return "", -1, base.ErrorResourceDoesNotExist
	}

	portsObjRaw, portsObjExists := externalObj[base.PortsKey]
	if !portsObjExists {
		return hostAddr, portNumber, portErr
	}

	portsObj, ok := portsObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalMgtHostAndPort: unable to cast portsObj to map[string]interface{}")
		return hostAddr, portNumber, portErr
	}

	var portKey string
	if isHttps {
		portKey = base.SSLMgtPortKey
	} else {
		portKey = base.MgtPortKey
	}

	mgmtObjRaw, mgmtObjExists := portsObj[portKey]
	if !mgmtObjExists {
		return hostAddr, portNumber, portErr
	}

	mgmtObj, ok := mgmtObjRaw.(float64)
	if !ok {
		u.logger_utils.Errorf("getExternalMgtHostAndPort: unable to cast mgmtObj to float64")
		return hostAddr, portNumber, portErr
	}

	portNumber = (int)(mgmtObj)
	portErr = nil
	return hostAddr, portNumber, portErr
}

// Returns remote node's SSL management port if it exists
func (u *Utilities) getExternalSSLMgtPort(nodeInfo map[string]interface{}) (int, error) {
	alternateObjRaw, alternateExists := nodeInfo[base.AlternateKey]
	if !alternateExists {
		return -1, base.ErrorResourceDoesNotExist
	}

	alternateObj, ok := alternateObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalSSLMgtPort: unable to cast alternateObj to map[string]interface{}")
		return -1, base.ErrorResourceDoesNotExist
	}

	externalObjRaw, externalExists := alternateObj[base.ExternalKey]
	if !externalExists {
		return -1, base.ErrorResourceDoesNotExist
	}

	externalObj, ok := externalObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalSSLMgtPort: unable to cast externalObj to map[string]interface{}")
		return -1, base.ErrorResourceDoesNotExist
	}

	portsObjRaw, portsObjExists := externalObj[base.PortsKey]
	if !portsObjExists {
		u.logger_utils.Warnf("Unable to convert portsObj to map[string]interface{}")
		return -1, base.ErrorNoPortNumber
	}

	portsObj, ok := portsObjRaw.(map[string]interface{})
	if !ok {
		return -1, base.ErrorNoPortNumber
	}

	mgmtSSLObjRaw, mgmtSSLExists := portsObj[base.SSLMgtPortKey]
	if !mgmtSSLExists {
		return -1, base.ErrorNoPortNumber
	}

	mgmtSSLObj, ok := mgmtSSLObjRaw.(float64)
	if !ok {
		u.logger_utils.Warnf("Unable to convert portsObj to float64")
		return -1, base.ErrorNoPortNumber
	}

	return (int)(mgmtSSLObj), nil
}

// Returns:
// 1. External Hostname
// 2. capi port
// 3. capi port error
// 4. capi SSL port
// 5. capi SSL port error
func (u *Utilities) getExternalHostAndCapiPorts(nodeInfo map[string]interface{}) (string, int, error, int, error) {
	var hostAddr string
	var capiPort int = -1
	var capiSSLPort int = -1
	var capiPortErr error = base.ErrorNoPortNumber
	var capiSSLPortErr error = base.ErrorNoPortNumber

	alternateObjRaw, alternateExists := nodeInfo[base.AlternateKey]
	if !alternateExists {
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	alternateObj, ok := (alternateObjRaw).(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalHostAndCapiPorts: Unable to convert alternateObj to map[string]interface{}")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	externalObjRaw, externalExists := alternateObj[base.ExternalKey]
	if !externalExists {
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	externalObj, ok := (externalObjRaw).(map[string]interface{})
	if !ok {
		u.logger_utils.Errorf("getExternalHostAndCapiPorts: Unable to convert externalObj to map[string]interface{}")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	hostAddrObjRaw, hostAddrObjExists := externalObj[base.HostNameKey]
	if !hostAddrObjExists {
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	hostAddr, ok = (hostAddrObjRaw).(string)
	if !ok {
		u.logger_utils.Errorf("getExternalHostAndCapiPorts: Unable to convert hostAddrObj to string")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	} else if len(hostAddr) == 0 {
		u.logger_utils.Errorf("getExternalHostAndCapiPorts: Empty Hostname")
		return "", -1, base.ErrorResourceDoesNotExist, -1, base.ErrorResourceDoesNotExist
	}

	portsObjRaw, portsObjExists := externalObj[base.PortsKey]
	if !portsObjExists {
		return "", -1, base.ErrorNoPortNumber, -1, base.ErrorNoPortNumber
	}

	portsObj, ok := portsObjRaw.(map[string]interface{})
	if !ok {
		u.logger_utils.Warnf("Unable to convert portsObj to map[string]interface{}")
	} else {
		capiPortRaw, capiPortExists := portsObj[base.CapiPortKey]
		if capiPortExists {
			portNumberFloat, ok := (capiPortRaw).(float64)
			if !ok {
				u.logger_utils.Warnf("Unable to convert capiPort to float64")
			} else {
				capiPort = (int)(portNumberFloat)
				capiPortErr = nil
			}
		}
		// Get the SSL port if it is there
		if sslPort, sslPortExists := portsObj[base.CapiSSLPortKey]; sslPortExists {
			sslPortNumberFloat, ok := sslPort.(float64)
			if !ok {
				u.logger_utils.Warnf("Unable to convert capiSSLPort to float64")
			} else {
				capiSSLPort = (int)(sslPortNumberFloat)
				capiSSLPortErr = nil
			}
		}
	}
	return hostAddr, capiPort, capiPortErr, capiSSLPort, capiSSLPortErr
}

func (u *Utilities) GetHostAddrFromNodeInfo(adminHostAddr string, nodeInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	hostAddr, err := u.getHostAddrFromNodeInfoInternal(adminHostAddr, nodeInfo, logger)
	if err == base.ErrorNoHostName {
		hostAddr = adminHostAddr
		err = nil
	}
	return hostAddr, err
}

func (u *Utilities) getHostAddrFromNodeInfoInternal(adminHostAddr string, nodeInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	var hostAddr string
	var ok bool

	hostAddrObj, ok := nodeInfo[base.HostNameKey]
	if !ok {
		logger.Infof("hostname is missing from node info %v. Host name in remote cluster reference, %v, will be used.\n", nodeInfo, adminHostAddr)
		return "", base.ErrorNoHostName
	} else {
		hostAddr, ok = hostAddrObj.(string)
		if !ok {
			return "", fmt.Errorf("Error getting host address from target cluster %v. host name, %v, is of wrong type\n", adminHostAddr, hostAddrObj)
		}
	}

	return hostAddr, nil
}

// Note - the translated map should be in the k->v form of:
// internalNodeAddress:directPort -> externalNodeAddress:kvPort
func (u *Utilities) GetIntExtHostNameKVPortTranslationMap(mapContainingNodesKey map[string]interface{}) (map[string]string, error) {
	internalExternalNodesMap := make(map[string]string)
	var err error
	var directPort int
	var nodesList []interface{}

	nodesList, err = u.GetNodeListFromInfoMap(mapContainingNodesKey, u.logger_utils)
	if err != nil {
		return internalExternalNodesMap, err
	}

	for _, nodeInfoRaw := range nodesList {
		nodeInfo, ok := nodeInfoRaw.(map[string]interface{})
		if !ok {
			u.logger_utils.Warnf("GetIntExtHostNameKVPortTranslationMap unable to cast nodeInfo as map[string]interface{} from: %v", nodeInfoRaw)
			// skip this node
			continue
		}

		internalAddressAndPortRaw, internalAddressOk := nodeInfo[base.HostNameKey]
		if !internalAddressOk {
			u.logger_utils.Warnf("GetIntExtHostNameKVPortTranslationMap unable to retrieve internal host name from %v", nodeInfo)
			// skip this node
			continue
		}

		internalAddressAndPort, ok := (internalAddressAndPortRaw).(string)
		if !ok {
			u.logger_utils.Warnf("GetIntExtHostNameKVPortTranslationMap unable to cast internalAddressAndPort as string: %v", internalAddressAndPortRaw)
			// skip this node
			continue
		}

		internalAddress := base.GetHostName(internalAddressAndPort)
		// Internally, we care about "direct" field
		portsObjRaw, portsExists := nodeInfo[base.PortsKey]
		if !portsExists {
			u.logger_utils.Warnf("Unable to get port for %v", internalAddress)
			// skip this node
			continue
		}
		portsObj, ok := portsObjRaw.(map[string]interface{})
		if !ok {
			u.logger_utils.Warnf("GetIntExtHostNameKVPortTranslationMap unable to cast portsObj as map[string]interface{} from: %v", portsObjRaw)
			// skip this node
			continue
		}

		directPortIface, directPortExists := portsObj[base.DirectPortKey]
		if !directPortExists {
			u.logger_utils.Warnf("Unable to get direct port for %v", internalAddress)
			// skip this node
			continue
		}
		directPortFloat, ok := directPortIface.(float64)
		if !ok {
			u.logger_utils.Warnf("GetIntExtHostNameKVPortTranslationMap unable to cast directPort as float", directPortIface)
			// skip this node
			continue
		}

		directPort = (int)(directPortFloat)
		internalAddressAndDirectPort := base.GetHostAddr(internalAddress, (uint16)(directPort))

		externalAddress, externalDirectPort, externalErr, _, _ := u.GetExternalAddressAndKvPortsFromNodeInfo(nodeInfo)
		if len(externalAddress) > 0 {
			if externalErr == nil {
				// External address and port both exist
				internalExternalNodesMap[internalAddressAndDirectPort] = base.GetHostAddr(externalAddress, (uint16)(externalDirectPort))
			} else if externalErr == base.ErrorNoPortNumber {
				// External address exists, but port does not. Use internal host's port number
				internalExternalNodesMap[internalAddressAndDirectPort] = base.GetHostAddr(externalAddress, (uint16)(directPort))
			}
		}
	}

	if len(internalExternalNodesMap) == 0 {
		err = base.ErrorResourceDoesNotExist
	}
	return internalExternalNodesMap, err
}

func (u *Utilities) GetHostNameFromNodeInfo(adminHostAddr string, nodeInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	hostAddr, err := u.GetHostAddrFromNodeInfo(adminHostAddr, nodeInfo, logger)
	if err != nil {
		return "", err
	}
	return base.GetHostName(hostAddr), nil
}

// this method is called when nodeInfo came from the terse bucket call, pools/default/b/[bucketName]
// where hostname in nodeInfo is a host name without port rather than a host address with port
func (u *Utilities) getHostNameWithoutPortFromNodeInfo(adminHostAddr string, nodeInfo map[string]interface{}, logger *log.CommonLogger) (string, error) {
	hostName, err := u.getHostAddrFromNodeInfoInternal(adminHostAddr, nodeInfo, logger)
	if err == base.ErrorNoHostName {
		hostName = base.GetHostName(adminHostAddr)
		err = nil
	}

	return hostName, err
}

//convenient api for rest calls to local cluster
func (u *Utilities) QueryRestApi(baseURL string,
	path string,
	preservePathEncoding bool,
	httpCommand string,
	contentType string,
	body []byte,
	timeout time.Duration,
	out interface{},
	logger *log.CommonLogger) (error, int) {
	return u.QueryRestApiWithAuth(baseURL, path, preservePathEncoding, "", "", base.HttpAuthMechPlain, nil, false, nil, nil, httpCommand, contentType, body, timeout, out, nil, false, logger)
}

func (u *Utilities) EnforcePrefix(prefix string, str string) string {
	var ret_str string = str
	if !strings.HasPrefix(str, prefix) {
		ret_str = prefix + str
	}
	return ret_str
}

func (u *Utilities) RemovePrefix(prefix string, str string) string {
	ret_str := strings.Replace(str, prefix, "", 1)
	return ret_str
}

//this expect the baseURL doesn't contain username and password
func (u *Utilities) QueryRestApiWithAuth(
	baseURL string,
	path string,
	preservePathEncoding bool,
	username string,
	password string,
	authMech base.HttpAuthMech,
	certificate []byte,
	san_in_certificate bool,
	clientCertificate []byte,
	clientKey []byte,
	httpCommand string,
	contentType string,
	body []byte,
	timeout time.Duration,
	out interface{},
	client *http.Client,
	keep_client_alive bool,
	logger *log.CommonLogger) (err error, statusCode int) {
	var http_client *http.Client
	if authMech != base.HttpAuthMechScramSha {
		var req *http.Request
		http_client, req, err = u.prepareForRestCall(baseURL, path, preservePathEncoding, username, password, authMech, certificate, san_in_certificate, clientCertificate, clientKey, httpCommand, contentType, body, client, logger)
		if err != nil {
			return
		}
		err, statusCode = u.doRestCall(req, timeout, out, http_client, logger)
	} else {
		err, statusCode, http_client = u.queryRestApiWithScramShaAuth(baseURL, path, preservePathEncoding, username, password, httpCommand, contentType, body, timeout, out, client, logger)

	}
	u.cleanupAfterRestCall(keep_client_alive, err, statusCode, http_client, logger)
	return
}

func (u *Utilities) queryRestApiWithScramShaAuth(
	baseURL string,
	path string,
	preservePathEncoding bool,
	username string,
	password string,
	httpCommand string,
	contentType string,
	body []byte,
	timeout time.Duration,
	out interface{},
	client *http.Client,
	logger *log.CommonLogger) (error, int, *http.Client) {

	logger.Debugf("SCRAM-SHA authentication for user %v%v%v, baseURL=%v, path=%v\n", base.UdTagBegin, username, base.UdTagEnd, baseURL, path)

	URL, err := u.constructURL(baseURL, path, preservePathEncoding, base.HttpAuthMechScramSha)
	if err != nil {
		return err, 0, nil
	}

	req, err := scramsha.NewRequest(httpCommand,
		// URL.String() is adequate since scramSha is always called with preservePathEncoding set to false
		URL.String(),
		strings.NewReader(string(body)))
	if err != nil {
		return err, 0, nil
	}

	if timeout == 0 {
		timeout = base.DefaultHttpTimeout
	}
	if client == nil {
		client = &http.Client{Timeout: timeout}
	} else {
		client.Timeout = timeout
	}

	res, err := scramsha.DoScramSha(req, username, password, client)
	statusCode := 0
	if res != nil {
		statusCode = res.StatusCode
	}
	if err != nil {
		return fmt.Errorf("Received error when making SCRAM-SHA connection. baseURL=%v, path=%v, err=%v", baseURL, path, err), statusCode, client
	}

	err = u.parseResponseBody(res, out, logger)
	return err, statusCode, client

}

func (u *Utilities) prepareForRestCall(baseURL string,
	path string,
	preservePathEncoding bool,
	username string,
	password string,
	authMech base.HttpAuthMech,
	certificate []byte,
	san_in_certificate bool,
	clientCertificate []byte,
	clientKey []byte,
	httpCommand string,
	contentType string,
	body []byte,
	client *http.Client,
	logger *log.CommonLogger) (*http.Client, *http.Request, error) {
	var l *log.CommonLogger = u.loggerForFunc(logger)
	var ret_client *http.Client = client

	userAuthMode := base.UserAuthModeNone

	if len(username) == 0 && len(clientCertificate) == 0 && path != base.SSLPortsPath {
		// username and clientCertificate can be both empty only when
		// 1. this is a local http call to the same node
		// or 2. this is a call to /nodes/self/xdcrSSLPorts on target to retrieve ssl port for subsequent https calls
		// treat case 1 separately, since we will need to set local user auth in http request
		userAuthMode = base.UserAuthModeLocal
	} else {
		// for http calls to remote target, set username and password in http request header if
		// 1. username has been provided
		// and 2. scram sha authentication is not used
		if len(username) != 0 && authMech != base.HttpAuthMechScramSha {
			userAuthMode = base.UserAuthModeBasic
		}
	}

	req, host, err := u.ConstructHttpRequest(baseURL, path, preservePathEncoding, username, password, authMech, userAuthMode, httpCommand, contentType, body, l)
	if err != nil {
		return nil, nil, err
	}

	if ret_client == nil {
		ret_client, err = u.GetHttpClient(username, authMech, certificate, san_in_certificate, clientCertificate, clientKey, host, l)
		if err != nil {
			l.Errorf("Failed to get client for request, err=%v, req=%v\n", err, req)
			return nil, nil, err
		}
	}
	return ret_client, req, nil
}

func (u *Utilities) cleanupAfterRestCall(keep_client_alive bool, err error, statusCode int, client *http.Client, logger *log.CommonLogger) {
	if !keep_client_alive || u.IsSeriousNetError(err) || u.isFatalStatusCode(statusCode) {
		if client != nil && client.Transport != nil {
			transport, ok := client.Transport.(*http.Transport)
			if ok {
				if u.IsSeriousNetError(err) {
					logger.Debugf("Encountered %v, close all idle connections for this http client.\n", err)
				}
				transport.CloseIdleConnections()
			}
		}
	}
}

func (u *Utilities) doRestCall(req *http.Request,
	timeout time.Duration,
	out interface{},
	client *http.Client,
	logger *log.CommonLogger) (error, int) {
	if timeout > 0 {
		client.Timeout = timeout
	} else if client.Timeout != base.DefaultHttpTimeout {
		client.Timeout = base.DefaultHttpTimeout
	}

	res, err := client.Do(req)
	if err == nil && res != nil {
		err = u.parseResponseBody(res, out, logger)
		return err, res.StatusCode
	}

	return err, 0

}

func (u *Utilities) parseResponseBody(res *http.Response,
	out interface{},
	logger *log.CommonLogger) error {
	var l *log.CommonLogger = u.loggerForFunc(logger)
	if res != nil && res.Body != nil {
		defer res.Body.Close()
		bod, e := ioutil.ReadAll(io.LimitReader(res.Body, res.ContentLength))
		if e != nil {
			l.Infof("Failed to read response body, err=%v\n res=%v\n", e, res)
			return e
		}
		if out != nil {
			err_marshal := json.Unmarshal(bod, out)
			if err_marshal != nil {
				l.Infof("Failed to unmarshal the response as json, err=%v, bod=%v\n res=%v\n", err_marshal, bod, res)
				out = bod
			} else {
				l.Debugf("out=%v\n", out)
			}
		} else {
			l.Debugf("out is nil")
		}
	}
	return nil
}

//convenient api for rest calls to local cluster
func (u *Utilities) InvokeRestWithRetry(baseURL string,
	path string,
	preservePathEncoding bool,
	httpCommand string,
	contentType string,
	body []byte,
	timeout time.Duration,
	out interface{},
	client *http.Client,
	keep_client_alive bool,
	logger *log.CommonLogger, num_retry int) (error, int) {
	return u.InvokeRestWithRetryWithAuth(baseURL, path, preservePathEncoding, "", "", base.HttpAuthMechPlain, nil, false, nil, nil, true, httpCommand, contentType, body, timeout, out, client, keep_client_alive, logger, num_retry)
}

func (u *Utilities) InvokeRestWithRetryWithAuth(baseURL string,
	path string,
	preservePathEncoding bool,
	username string,
	password string,
	authMech base.HttpAuthMech,
	certificate []byte,
	san_in_certificate bool,
	clientCertificate []byte,
	clientKey []byte,
	insecureSkipVerify bool,
	httpCommand string,
	contentType string,
	body []byte,
	timeout time.Duration,
	out interface{},
	client *http.Client,
	keep_client_alive bool,
	logger *log.CommonLogger, num_retry int) (err error, statusCode int) {

	var http_client *http.Client = nil
	var req *http.Request = nil
	backoff_time := 500 * time.Millisecond

	for i := 0; i < num_retry; i++ {
		if authMech != base.HttpAuthMechScramSha {
			http_client, req, err = u.prepareForRestCall(baseURL, path, preservePathEncoding, username, password, authMech, certificate, san_in_certificate, clientCertificate, clientKey, httpCommand, contentType, body, client, logger)
			if err == nil {
				err, statusCode = u.doRestCall(req, timeout, out, http_client, logger)
			}

			if err == nil {
				break
			}
		} else {
			err, statusCode, http_client = u.queryRestApiWithScramShaAuth(baseURL, path, preservePathEncoding, username, password, httpCommand, contentType, body, timeout, out, client, logger)
			if err == nil {
				break
			}
		}

		logger.Infof("Received error when making rest call. baseURL=%v, path=%v, err=%v, statusCode=%v, num_retry=%v\n", baseURL, path, err, statusCode, i)

		//cleanup the idle connection if the error is serious network error
		u.cleanupAfterRestCall(true /*keep_client_alive*/, err, statusCode, http_client, logger)

		//backoff
		backoff_time = backoff_time + backoff_time
		time.Sleep(backoff_time)
	}

	return

}

func (u *Utilities) GetHttpClient(username string, authMech base.HttpAuthMech, certificate []byte, san_in_certificate bool, clientCertificate, clientKey []byte, ssl_con_str string, logger *log.CommonLogger) (*http.Client, error) {
	var client *http.Client
	if authMech == base.HttpAuthMechHttps {
		caPool := x509.NewCertPool()
		ok := caPool.AppendCertsFromPEM(certificate)
		if !ok {
			return nil, base.InvalidCerfiticateError
		}

		//using a separate tls connection to verify certificate
		//it can be changed in 1.4 when DialTLS is avaialbe in http.Transport
		conn, tlsConfig, err := base.MakeTLSConn(ssl_con_str, username, certificate, san_in_certificate, clientCertificate, clientKey, logger)
		if err != nil {
			return nil, err
		}
		conn.Close()

		tr := &http.Transport{TLSClientConfig: tlsConfig, Dial: base.DialTCPWithTimeout}
		client = &http.Client{Transport: tr,
			Timeout: base.DefaultHttpTimeout}

	} else {
		client = &http.Client{Timeout: base.DefaultHttpTimeout}
	}
	return client, nil
}

//this expect the baseURL doesn't contain username and password
func (u *Utilities) ConstructHttpRequest(
	baseURL string,
	path string,
	preservePathEncoding bool,
	username string,
	password string,
	authMech base.HttpAuthMech,
	userAuthMode base.UserAuthMode,
	httpCommand string,
	contentType string,
	body []byte,
	logger *log.CommonLogger) (*http.Request, string, error) {
	url, err := u.constructURL(baseURL, path, preservePathEncoding, authMech)
	if err != nil {
		return nil, "", err
	}

	var l *log.CommonLogger = u.loggerForFunc(logger)

	req, err := http.NewRequest(httpCommand, url.String(), bytes.NewBuffer(body))
	if err != nil {
		return nil, "", err
	}

	if preservePathEncoding {
		// get the original Opaque back
		req.URL.Opaque = url.Opaque
	}

	if contentType == "" {
		contentType = base.DefaultContentType
	}
	req.Header.Set(base.ContentType, contentType)

	req.Header.Set(base.UserAgent, base.GoxdcrUserAgent)

	switch userAuthMode {
	case base.UserAuthModeLocal:
		err := cbauth.SetRequestAuth(req)
		if err != nil {
			l.Errorf("Failed to set authentication to request. err=%v\n req=%v\n", err, req)
			return nil, "", err
		}
	case base.UserAuthModeBasic:
		req.SetBasicAuth(username, password)
	case base.UserAuthModeNone:
		// no op
	default:
		return nil, "", fmt.Errorf("Invalid userAuthMode %v", userAuthMode)
	}

	//TODO: log request would log password barely
	l.Debugf("http request=%v\n", req)

	return req, url.Host, nil
}

func (u *Utilities) constructURL(baseURL string,
	path string,
	preservePathEncoding bool,
	authMech base.HttpAuthMech) (*url.URL, error) {

	var baseURL_new string
	if authMech == base.HttpAuthMechHttps {
		baseURL_new = u.EnforcePrefix("https://", baseURL)
	} else {
		baseURL_new = u.EnforcePrefix("http://", baseURL)
	}
	url, err := couchbase.ParseURL(baseURL_new)
	if err != nil {
		return nil, err
	}

	if !preservePathEncoding {
		if q := strings.Index(path, "?"); q > 0 {
			url.Path = path[:q]
			url.RawQuery = path[q+1:]
		} else {
			url.Path = path
		}
	} else {
		// use url.Opaque to preserve encoding
		url.Opaque = "//"

		index := strings.Index(baseURL_new, "//")
		if index < len(baseURL_new)-2 {
			url.Opaque += baseURL_new[index+2:]
		}
		url.Opaque += path
	}
	return url, nil
}

// encode http request into wire format
// it differs from HttpRequest.Write() in that it preserves the Content-Length in the header,
// and ignores Body in request
func (u *Utilities) EncodeHttpRequest(req *http.Request) ([]byte, error) {
	reqBytes := make([]byte, 0)
	reqBytes = append(reqBytes, []byte(req.Method)...)
	reqBytes = append(reqBytes, []byte(" ")...)
	reqBytes = append(reqBytes, []byte(req.URL.String())...)
	reqBytes = append(reqBytes, []byte(" HTTP/1.1\r\n")...)

	hasHost := false
	for key, value := range req.Header {
		if key == "Host" {
			hasHost = true
		}
		if value != nil && len(value) > 0 {
			reqBytes = u.EncodeHttpRequestHeader(reqBytes, key, value[0])
		} else {
			reqBytes = u.EncodeHttpRequestHeader(reqBytes, key, "")
		}
	}
	if !hasHost {
		// ensure that host name is in header
		reqBytes = u.EncodeHttpRequestHeader(reqBytes, "Host", req.Host)
	}

	// add extra "\r\n" as separator for Body
	reqBytes = append(reqBytes, []byte("\r\n")...)

	if req.Body != nil {
		defer req.Body.Close()

		bodyBytes, err := ioutil.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		reqBytes = append(reqBytes, bodyBytes...)
	}
	return reqBytes, nil
}

func (u *Utilities) EncodeHttpRequestHeader(reqBytes []byte, key, value string) []byte {
	reqBytes = append(reqBytes, []byte(key)...)
	reqBytes = append(reqBytes, []byte(": ")...)
	reqBytes = append(reqBytes, []byte(value)...)
	reqBytes = append(reqBytes, []byte("\r\n")...)
	return reqBytes
}

func (u *Utilities) IsSeriousNetError(err error) bool {
	if err == nil {
		return false
	}

	errStr := err.Error()
	netError, ok := err.(*net.OpError)
	return err == syscall.EPIPE ||
		err == io.EOF ||
		strings.Contains(errStr, "EOF") ||
		strings.Contains(errStr, "use of closed network connection") ||
		strings.Contains(errStr, "connection reset by peer") ||
		strings.Contains(errStr, "http: can't write HTTP request on broken connection") ||
		(ok && (!netError.Temporary() && !netError.Timeout()))
}

// statusCode that requires connections to be dropped and recreated
// basically all statusCodes that are larger than 400 are fatal
func (u *Utilities) isFatalStatusCode(statusCode int) bool {
	return statusCode > http.StatusBadRequest
}

func (u *Utilities) NewTCPConn(hostName string) (*net.TCPConn, error) {
	conn, err := base.DialTCPWithTimeout(base.NetTCP, hostName)
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("Failed to set up connection to %v", hostName)
	}
	tcpConn, ok := conn.(*net.TCPConn)
	if !ok {
		// should never get here
		conn.Close()
		return nil, fmt.Errorf("The connection to %v returned is not TCP type", hostName)
	}

	// same settings as erlang xdcr
	err = tcpConn.SetKeepAlive(true)
	if err == nil {
		err = tcpConn.SetKeepAlivePeriod(base.KeepAlivePeriod)
	}
	if err == nil {
		err = tcpConn.SetNoDelay(false)
	}

	if err != nil {
		tcpConn.Close()
		return nil, fmt.Errorf("Error setting options on the connection to %v. err=%v", hostName, err)
	}

	return tcpConn, nil
}

/**
 * Executes a anonymous function that returns an error. If the error is non nil, retry with exponential backoff.
 * Returns base.ErrorFailedAfterRetry + the last recorded error if operation times out, nil otherwise.
 * Max retries == the times to retry in additional to the initial try, should the initial try fail
 * initialWait == Initial time with which to start
 * Factor == exponential backoff factor based off of initialWait
 */
func (u *Utilities) ExponentialBackoffExecutor(name string, initialWait time.Duration, maxRetries int, factor int, op ExponentialOpFunc) error {
	waitTime := initialWait
	var opErr error
	for i := 0; i <= maxRetries; i++ {
		opErr = op()
		if opErr == nil {
			return nil
		} else if i != maxRetries {
			u.logger_utils.Warnf("ExponentialBackoffExecutor for %v encountered error (%v). Sleeping %v\n",
				name, opErr.Error(), waitTime)
			time.Sleep(waitTime)
			waitTime *= time.Duration(factor)
		}
	}
	opErr = fmt.Errorf("%v %v Last error: %v", name, base.ErrorFailedAfterRetry.Error(), opErr.Error())
	return opErr
}

/*
 * This method has an additional parameter, finCh, than ExponentialBackoffExecutor. When finCh is closed,
 * this method can abort earlier.
 */
func (u *Utilities) ExponentialBackoffExecutorWithFinishSignal(name string, initialWait time.Duration, maxRetries int, factor int, op ExponentialOpFunc2, param interface{}, finCh chan bool) (interface{}, error) {
	waitTime := initialWait
	var result interface{}
	var err error
	for i := 0; i <= maxRetries; i++ {
		select {
		case <-finCh:
			err = fmt.Errorf("ExponentialBackoffExecutorWithFinishSignal for %v aborting because of finch closure\n", name)
			u.logger_utils.Warnf(err.Error())
			return nil, err
		default:
			result, err = op(param)
			if err == nil {
				return result, nil
			} else if i != maxRetries {
				u.logger_utils.Warnf("ExponentialBackoffExecutorWithFinishSignal for %v encountered error (%v). Sleeping %v\n",
					name, err.Error(), waitTime)
				base.WaitForTimeoutOrFinishSignal(waitTime, finCh)
				waitTime *= time.Duration(factor)
			}
		}
	}
	err = fmt.Errorf("%v %v Last error: %v", name, base.ErrorFailedAfterRetry.Error(), err.Error())
	return nil, err
}

// Get security related settings from target
// 1. whether target supports SAN in certificate
// 2. authentication mechanism to use when making http[s] connections to target ns_server
// This method used ShortHttpTimeout because it is either called from remote cluster rest API,
// where a prompt response is required to keep the rest request from timing out,
// or called from remote cluster reference refresh code, where a pre-mature timeout can be tolerated
// This method also returns defaultPoolInfo of target for more flexibility
// CALLER BEWARE: defaultPoolInfo is returned ONLY when either scram sha or ssl is enabled, so as to avoid unnecessary work
func (u *Utilities) GetSecuritySettingsAndDefaultPoolInfo(hostAddr, hostHttpsAddr, username, password string,
	certificate []byte, clientCertificate, clientKey []byte, scramShaEnabled bool, logger *log.CommonLogger) (sanInCertificate bool,
	httpAuthMech base.HttpAuthMech, defaultPoolInfo map[string]interface{}, err error) {
	if !scramShaEnabled && len(certificate) == 0 {
		// security settings are irrelevant if we are not using scram sha or ssl
		// note that a nil defaultPoolInfo is returned in this case
		return false, base.HttpAuthMechPlain, nil, nil
	}

	if scramShaEnabled {
		// if scram sha is enabled, we will first try to connect to target ns_server using scram sha authentication
		// even if certificate/clientCert have been provided, we will not use them here because they are not needed by scram sha authentication
		defaultPoolInfo, err = u.getDefaultPoolInfoUsingScramSha(hostAddr, username, password, logger)
		if err == nil {
			httpAuthMech = base.HttpAuthMechScramSha
		} else if err != TargetMayNotSupportScramShaError {
			return false, base.HttpAuthMechPlain, nil, err
		} else {
			if len(certificate) == 0 {
				// certificate not provided, cannot fall back to https. return error right away
				return false, base.HttpAuthMechPlain, nil, fmt.Errorf("Cannot connect to target %v using \"half\" secure mode. Received unauthorized error when using Scram-Sha authentication. Cannot use https because server certificate has not been provided.", hostAddr)
			} else {
				// proceed to fall back to https
			}
		}
	}

	if defaultPoolInfo == nil {
		// if we get here, either scram sha is not enabled, or scram sha is enabled and target ns_server returned 401 error on our scram sha attempt
		// either way, it is implied that certificate has been provided. use https to connect to target
		defaultPoolInfo, err = u.getDefaultPoolInfoUsingHttps(hostHttpsAddr, username, password,
			certificate, clientCertificate, clientKey, logger)
		if err == nil {
			httpAuthMech = base.HttpAuthMechHttps
		} else {
			return false, base.HttpAuthMechPlain, nil, err
		}
	}

	// at this point, we have a properly set httpAuthMech and a valid defaultPoolInfo
	// derive certificate related settings from defaultPoolInfo

	nodeList, err := u.GetNodeListFromInfoMap(defaultPoolInfo, logger)
	if err != nil || len(nodeList) == 0 {
		err = fmt.Errorf("Can't get nodes information for cluster %v, err=%v", hostAddr, err)
		return false, base.HttpAuthMechPlain, nil, err
	}

	clusterCompatibility, err := u.GetClusterCompatibilityFromNodeList(nodeList)
	if err != nil {
		return false, base.HttpAuthMechPlain, nil, err
	}

	targetHasScramShaSupport := base.IsClusterCompatible(clusterCompatibility, base.VersionForHttpScramShaSupport)
	if scramShaEnabled && targetHasScramShaSupport && httpAuthMech != base.HttpAuthMechScramSha {
		// do not fall back to https if target is vulcan and up
		return false, base.HttpAuthMechPlain, nil, fmt.Errorf("Failed to retrieve secruity settings from host=%v using SCRAM-SHA authentication. Please check whether SCRAM-SHA is enabled on target.", hostAddr)
	}

	sanInCertificate = base.IsClusterCompatible(clusterCompatibility, base.VersionForSANInCertificateSupport)
	return sanInCertificate, httpAuthMech, defaultPoolInfo, nil
}

func (u *Utilities) getDefaultPoolInfoUsingScramSha(hostAddr, username, password string, logger *log.CommonLogger) (map[string]interface{}, error) {
	defaultPoolInfo := make(map[string]interface{})
	err, statusCode := u.QueryRestApiWithAuth(hostAddr, base.DefaultPoolPath, false, username, password, base.HttpAuthMechScramSha, nil /*certificate*/, false /*sanInCertificate*/, nil /*clientCertificate*/, nil /*clientKey*/, base.MethodGet, "", nil, base.ShortHttpTimeout, &defaultPoolInfo, nil, false, logger)
	if err == nil && statusCode == http.StatusOK {
		// target supports scram sha
		return defaultPoolInfo, nil
	} else if statusCode == http.StatusUnauthorized {
		// unauthorized error could be returned when target ns_server is pre-vulcan and does not support scram sha.
		// return a specific error to allow caller to fall back to https
		return nil, TargetMayNotSupportScramShaError
	} else {
		return nil, fmt.Errorf("Failed to retrieve secruity settings from host=%v using scram sha, err=%v, statusCode=%v", hostAddr, err, statusCode)
	}
}

func (u *Utilities) getDefaultPoolInfoUsingHttps(hostHttpsAddr, username, password string,
	certificate []byte, clientCertificate, clientKey []byte, logger *log.CommonLogger) (map[string]interface{}, error) {
	defaultPoolInfo := make(map[string]interface{})

	// we do not know the correct values of sanInCertificate. set sanInCertificate set to true for better security
	err, statusCode := u.QueryRestApiWithAuth(hostHttpsAddr, base.DefaultPoolPath, false, username, password, base.HttpAuthMechHttps, certificate, true /*sanInCertificate*/, clientCertificate, clientKey, base.MethodGet, "", nil, base.ShortHttpTimeout, &defaultPoolInfo, nil, false, logger)
	if err == nil && statusCode == http.StatusOK {
		return defaultPoolInfo, nil
	} else {
		if err != nil && strings.Contains(err.Error(), base.NoIpSANErrMsg) {
			// if the error is about certificate not containing IP SANs, it could be that the target cluster is of an old version
			// make a second try with sanInCertificate set to false
			// after we retrieve target cluster version, we will then re-set sanInCertificate to the appropriate value
			logger.Debugf("Received certificate validation error from %v. Target may be an old version that does not support SAN in certificates. Retrying connection to target using sanInCertificate = false.", hostHttpsAddr)
			err, statusCode = u.QueryRestApiWithAuth(hostHttpsAddr, base.DefaultPoolPath, false, username, password, base.HttpAuthMechHttps, certificate, false /*sanInCertificate*/, clientCertificate, clientKey, base.MethodGet, "", nil, base.ShortHttpTimeout, &defaultPoolInfo, nil, false, logger)
			if err == nil && statusCode == http.StatusOK {
				return defaultPoolInfo, nil
			} else {
				// if the second try still fails, return error
				return nil, fmt.Errorf("Failed to retrieve secruity settings from host=%v, err=%v, statusCode=%v %v", hostHttpsAddr, err, statusCode, u.getAdditionalErrorMessage(statusCode, username))
			}
		} else {
			return nil, fmt.Errorf("Failed to retrieve secruity settings from host=%v, err=%v, statusCode=%v %v", hostHttpsAddr, err, statusCode, u.getAdditionalErrorMessage(statusCode, username))
		}
	}
}

// Given the KVVBMap, translate the map so that the server keys are replaced with external server keys, if applicable
func (u *Utilities) TranslateKvVbMap(kvVBMap base.BucketKVVbMap, targetBucketInfo map[string]interface{}) {
	translationMap, translationErr := u.GetIntExtHostNameKVPortTranslationMap(targetBucketInfo)
	if translationErr != nil && translationErr != base.ErrorResourceDoesNotExist {
		u.logger_utils.Warnf("Error constructing internal -> external address translation table. err=%v", translationErr)
	} else if translationErr == nil {
		(base.BucketKVVbMap)(kvVBMap).ReplaceInternalWithExternalHosts(translationMap)
	}
}

func (u *Utilities) ReplaceCouchApiBaseObjWithExternals(couchApiBase string, nodeInfo map[string]interface{}) string {
	if len(couchApiBase) == 0 {
		return couchApiBase
	}

	extHost, extCapi, extCapiErr, extCapiSSL, extCapiSSLErr := u.getExternalHostAndCapiPorts(nodeInfo)
	if len(extHost) > 0 {
		// "couchApiBaseHTTPS": "https://127.0.0.1:19502/b2%2B746a570d364cf609ac11572f8c8c2608",
		url, err := url.Parse(couchApiBase)
		if err != nil || !url.IsAbs() {
			u.logger_utils.Errorf("Unable to parse URL string for CouchApiBase: %v", err)
			return couchApiBase
		}
		var isHttps bool = strings.HasPrefix(couchApiBase, "https")
		var leadingHttpString string
		if isHttps {
			leadingHttpString = "https://"
		} else {
			leadingHttpString = "http://"
		}

		// Now strip out the http(s)://host:port/
		var leadingPrefix string
		leadingHostName := url.Hostname()
		leadingPort := url.Port()
		if len(leadingPort) > 0 {
			leadingPrefix = fmt.Sprintf("%s%s:%s/", leadingHttpString, leadingHostName, leadingPort)
		} else {
			leadingPrefix = fmt.Sprintf("%s%s/", leadingHttpString, leadingHostName)
		}
		strippedCouchApiBase := strings.TrimPrefix(couchApiBase, leadingPrefix)

		// Now recompile
		var recompiledUrl string
		var recompiledHostToUse string = extHost
		var recompiledPortToUse string
		if isHttps && extCapiSSLErr == nil {
			recompiledPortToUse = fmt.Sprintf("%v", extCapiSSL)
		} else if !isHttps && extCapiErr == nil {
			recompiledPortToUse = fmt.Sprintf("%v", extCapi)
		} else {
			recompiledPortToUse = leadingPort
		}
		if len(recompiledPortToUse) == 0 {
			recompiledUrl = fmt.Sprintf("%s%s/%s", leadingHttpString, recompiledHostToUse, strippedCouchApiBase)
		} else {
			recompiledUrl = fmt.Sprintf("%s%s:%s/%s", leadingHttpString, recompiledHostToUse, recompiledPortToUse, strippedCouchApiBase)
		}
		return recompiledUrl
	}
	return couchApiBase
}

func (u *Utilities) getAdditionalErrorMessage(statusCode int, username string) string {
	errMsg := ""
	if statusCode == http.StatusUnauthorized {
		errMsg = "(Received unauthorized error from target. Please double check user credentials."
		// if username has not been specified [implying that client certificate has been provided and is being used]
		// unauthorized error could also be returned if target has client cert auth setting set to disable
		if len(username) == 0 {
			errMsg += " Since client certificate is being used, please ensure that target is version 5.5 and up and has client certificate authentication setting set to \"enable\" or \"mandatory\".)"
		} else {
			errMsg += ")"
		}
	}
	return errMsg
}

func decompressSnappyBody(incomingBody, key []byte, dp DataPoolIface, slicesToBeReleased *[][]byte) ([]byte, error, string, int64, int) {
	var dpFailedCnt int64
	lenOfDecodedData, err := snappy.DecodedLen(incomingBody)
	lastBodyPos := lenOfDecodedData - 1
	if err != nil {
		return nil, base.ErrorCompressionUnableToInflate, fmt.Sprintf("XDCR for key %v%v%v is unable to decode snappy uncompressed size: %v", base.UdTagBegin, string(key), base.UdTagEnd, err), dpFailedCnt, lastBodyPos
	}

	uncompressedBodySize := uint64(lenOfDecodedData + len(key) + base.AddFilterKeyExtraBytes)
	body, err := dp.GetByteSlice(uncompressedBodySize)
	if err != nil {
		body = make([]byte, 0, uncompressedBodySize)
		dpFailedCnt = int64(uncompressedBodySize)
	} else {
		*slicesToBeReleased = append(*slicesToBeReleased, body)
	}

	body, err = snappy.Decode(body, incomingBody)
	if err != nil {
		return nil, base.ErrorCompressionUnableToInflate, fmt.Sprintf("XDCR for key %v%v%v is unable to snappy decompress body value: %v", base.UdTagBegin, string(key), base.UdTagEnd, err), dpFailedCnt, lastBodyPos
	}

	// Check to make sure the last bracket position is correct
	if body[lastBodyPos] != '}' {
		return nil, base.ErrorInvalidInput, fmt.Sprintf("XDCR for key %v%v%v after decompression seems to be an invalid JSON", base.UdTagBegin, string(key), base.UdTagEnd), dpFailedCnt, lastBodyPos
	}

	return body, err, "", dpFailedCnt, lastBodyPos
}

func getBodySlice(incomingBody, key []byte, dp DataPoolIface, slicesToBeReleased *[][]byte) ([]byte, error, string, int64, int) {
	var dpFailedCnt int64
	var incomingBodyLen int = len(incomingBody)
	lastBodyPos := incomingBodyLen - 1
	bodySize := uint64(incomingBodyLen + len(key) + base.AddFilterKeyExtraBytes)

	if incomingBody[lastBodyPos] != '}' {
		return nil, base.ErrorInvalidInput, fmt.Sprintf("Document %v%v%v body is not a valid JSON", base.UdTagBegin, string(key), base.UdTagEnd), dpFailedCnt, lastBodyPos
	}

	body, err := dp.GetByteSlice(bodySize)
	if err != nil {
		body = make([]byte, 0, bodySize)
		dpFailedCnt = int64(bodySize)
	}
	*slicesToBeReleased = append(*slicesToBeReleased, body)
	copy(body, incomingBody)
	return body, nil, "", dpFailedCnt, lastBodyPos
}

func stripAndPrependXattribute(body, key []byte, xattrSize uint32, dp DataPoolIface, slicesToBeReleased *[][]byte, endBodyPos int) ([]byte, error, int64, int) {
	var dpFailedCnt int64
	actualBody := body[xattrSize+4:]
	endBodyPos = endBodyPos - int(xattrSize) - 4

	bodySize := uint64(int(xattrSize) + 4 + len(body) + base.AddFilterXattrExtraBytes)

	combinedBody, err := dp.GetByteSlice(bodySize)
	if err != nil {
		combinedBody = make([]byte, 0, bodySize)
		dpFailedCnt += int64(bodySize)
	} else {
		*slicesToBeReleased = append(*slicesToBeReleased, combinedBody)
	}

	// Prereq check
	if actualBody[0] != '{' {
		return nil, base.ErrorInvalidInput, dpFailedCnt, endBodyPos
	}

	// Current body looks like (spaces added for readability):
	// { key : val }
	// Want to insert xattr at the beginning so "combinedBody" looks like:
	// { XdcrInternalXattrKey : { xattrKey : xattrVal } , key : val }

	// { XdcrInternalXattrKey :
	combinedBodyPos := 0
	combinedBody, combinedBodyPos = base.WriteJsonRawMsg(combinedBody, base.CachedInternalKeyXattrByteSlice, combinedBodyPos, true /*key*/, base.CachedInternalKeyXattrByteSize, false /*needQuotes*/, combinedBodyPos == 0 /*firstKey*/)

	// { XdcrInternalXattrKey : { xattrKey : xattrVal }
	// Followed by uint32 -> key -> NUL -> value -> NUL (repeat)
	var pos uint32 = 4 // skip the first 4 bytes, which is the size of the xattr
	var separator uint32
	for pos < xattrSize+4 {
		pos = pos + 4

		// Search for end of key
		for separator = pos; body[separator] != '\x00'; separator++ {
			if separator >= xattrSize+4 {
				return nil, fmt.Errorf("For document %v%v%v, Unable to correctly parse xattr to find a xattr key", base.UdTagBegin, string(key), base.UdTagEnd), dpFailedCnt, endBodyPos
			}
		}
		// Note the first time through this loop, pos == 8
		combinedBody, combinedBodyPos = base.WriteJsonRawMsg(combinedBody, body[pos:separator], combinedBodyPos, true /*key*/, len(body[pos:separator]), false /*needsQuotes*/, pos == 8 /*firstKey*/)
		pos = separator + 1

		// Search for end of value
		for separator = pos; body[separator] != '\x00'; separator++ {
			if separator >= xattrSize+4 {
				return nil, fmt.Errorf("For document %v%v%v, Unable to correctly parse xattr to find value for xattr key %v%v%v", base.UdTagBegin, string(key), base.UdTagEnd, base.UdTagBegin, key, base.UdTagEnd), dpFailedCnt, endBodyPos
			}
		}
		combinedBody, combinedBodyPos = base.WriteJsonRawMsg(combinedBody, body[pos:separator], combinedBodyPos, false, len(body[pos:separator]), false, false)

		// separator should be pointing at a 0 byte. Next uint32 is after this byte
		pos = separator + 1
	}
	// Currently:                                     v - combinedBodyPos
	// { XdcrInternalXattrKey : { xattrKey : xattrVal }

	// Targeted:                                        v - combinedBodyPos
	// { XdcrInternalXattrKey : { xattrKey : xattrVal },
	combinedBodyPos++
	combinedBody[combinedBodyPos] = ','
	// endBodyPos is added instead of combinedBodyPos+1 is because below we are copying actualBody[1:] to skip the first {
	endBodyPos += combinedBodyPos
	combinedBodyPos++

	// ActualBody:
	// { key : val }
	// targeted combinedBody:
	// { XdcrInternalXattrKey : { xattrKey : xattrVal }, key : val }
	copy(combinedBody[combinedBodyPos:], actualBody[1:])

	return combinedBody, nil, dpFailedCnt, endBodyPos
}

func processXattribute(body, key []byte, shouldSkipInsert bool, dp DataPoolIface, slicesToBeReleased *[][]byte, endBodyPos int) ([]byte, error, int64, int) {
	var pos uint32
	//	var separator uint32
	var dpFailedCnt int64
	var failedCnt int64
	var err error

	//	first uint32 in the body contains the size of the entire XATTR section
	totalXattrSize := binary.BigEndian.Uint32(body[pos : pos+4])
	// Couchbase doc size is max of 20MB. Xattribute count against this limit.
	// So if total xattr size is greater than this limit, then something is wrong
	if totalXattrSize > base.MaxDocSizeByte {
		return nil, fmt.Errorf("For document %v%v%v, unable to correctly parse xattribute from DCP packet. Xattr size determined to be %v bytes, which is invalid", base.UdTagBegin, string(key), base.UdTagEnd, totalXattrSize), dpFailedCnt, endBodyPos
	}

	if shouldSkipInsert {
		// TotalXattrSize does not account the last NUL char (4 bytes of '\x00') in the else branch's logic
		newBody := body[totalXattrSize+4:]
		body = newBody
		endBodyPos = endBodyPos - int(totalXattrSize) - 4
	} else {
		body, err, failedCnt, endBodyPos = stripAndPrependXattribute(body, key, totalXattrSize, dp, slicesToBeReleased, endBodyPos)
		if failedCnt > 0 {
			dpFailedCnt += failedCnt
		}
	}
	return body, err, dpFailedCnt, endBodyPos
}

func processKeyOnlyForFiltering(key []byte, dp DataPoolIface, slicesToBeReleased *[][]byte) ([]byte, int64) {
	var body []byte
	// "key"
	docKeySize := uint64(len(key) + 2)
	newDocKeyBody, err := dp.GetByteSlice(docKeySize)
	if err == nil {
		*slicesToBeReleased = append(*slicesToBeReleased, newDocKeyBody)
		// {  : " " } <- 5
		bodySize := docKeySize + uint64(5+len(base.ReservedWordsMap[base.ExternalKeyKey]))
		body, err = dp.GetByteSlice(bodySize)
	}
	if err != nil {
		// If there is any problem using datapool, just use json.RawMessage directly to allocate new byte slice
		body = json.RawMessage(fmt.Sprintf("{\"%v\":\"%v\"}", base.ReservedWordsMap[base.ExternalKeyKey], string(key)))
		return body, int64(len(body))
	}

	*slicesToBeReleased = append(*slicesToBeReleased, body)
	// Because we are using UprEvent.Key as a body, and the string hasn't been serialized,
	// we need to enclose it in " "
	newDocKeyBody[0] = '"'
	copy(newDocKeyBody[1:], key[:])
	newDocKeyBody[len(key)+1] = '"'

	var bodyPos int
	body, bodyPos = base.WriteJsonRawMsg(body, base.CachedInternalKeyKeyByteSlice, bodyPos, true /*key*/, base.CachedInternalKeyKeyByteSize, false, bodyPos == 0)
	body, bodyPos = base.WriteJsonRawMsg(body, newDocKeyBody, bodyPos, false /*uprEvent key as value*/, int(docKeySize), false /*valueNeedsQuotes*/, false /*firstKey*/)
	return body, 0
}

func (u *Utilities) ProcessUprEventForFiltering(uprEvent *mcc.UprEvent, dp DataPoolIface, flags base.FilterFlagType, slicesToBeReleased *[][]byte) ([]byte, error, string, ReleaseMemFunc, int64) {
	var body []byte
	var err error
	var additionalErrDesc string
	var totalFailedCnt int64
	var endBodyPos int

	if uprEvent == nil || dp == nil {
		return nil, base.ErrorInvalidInput, additionalErrDesc, nil, totalFailedCnt
	}

	// We cannot tell how much Xattr is using until we deflate - so we have to just allocate >1 slices if
	// that's the case
	releaseFunc := func() {
		for _, aSlice := range *slicesToBeReleased {
			dp.PutByteSlice(aSlice)
		}
		*slicesToBeReleased = (*slicesToBeReleased)[:0]
	}

	// Simplify things
	bodyContainsXattr := uprEvent.DataType&mcc.XattrDataType > 0
	shouldSkipXattr := flags&base.FilterFlagSkipXattr > 0
	needToProcessBody := flags&base.FilterFlagKeyOnly == 0 && (bodyContainsXattr || uprEvent.DataType&mcc.JSONDataType > 0)
	shouldSkipKey := flags&base.FilterFlagSkipKey > 0
	bodyIsCompressed := uprEvent.DataType&mcc.SnappyDataType > 0

	if needToProcessBody {
		if bodyIsCompressed {
			body, err, additionalErrDesc, totalFailedCnt, endBodyPos = decompressSnappyBody(uprEvent.Value, uprEvent.Key, dp, slicesToBeReleased)
			if err != nil {
				return nil, err, additionalErrDesc, releaseFunc, totalFailedCnt
			}
		} else {
			body, err, additionalErrDesc, totalFailedCnt, endBodyPos = getBodySlice(uprEvent.Value, uprEvent.Key, dp, slicesToBeReleased)
			if err != nil {
				return nil, err, additionalErrDesc, releaseFunc, totalFailedCnt
			}
		}

		if bodyContainsXattr {
			var failedCnt int64
			body, err, failedCnt, endBodyPos = processXattribute(body, uprEvent.Key, shouldSkipXattr, dp, slicesToBeReleased, endBodyPos)
			if failedCnt > 0 {
				totalFailedCnt += failedCnt
			}
			if err != nil {
				additionalErrDesc = fmt.Sprintf("For document %v%v%v Unable to parse xattribute: %v", base.UdTagBegin, string(uprEvent.Key), base.UdTagEnd, err)
				return nil, base.ErrorFilterParsingError, additionalErrDesc, releaseFunc, totalFailedCnt
			}
		}
	}

	if !shouldSkipKey {
		var failedCnt int64
		if !needToProcessBody {
			// Only thing passing to filter is the document key
			body, failedCnt = processKeyOnlyForFiltering(uprEvent.Key, dp, slicesToBeReleased)
			if failedCnt > 0 {
				totalFailedCnt += failedCnt
			}
		} else {
			// Add Key to Body
			body, err, failedCnt = base.AddKeyToBeFiltered(body, uprEvent.Key, dp.GetByteSlice, slicesToBeReleased, endBodyPos)
			if failedCnt > 0 {
				totalFailedCnt += failedCnt
			}
			if err != nil {
				additionalErrDesc = fmt.Sprintf("For document %v%v%v Unable to add key to body as the body may be malformed JSON", base.UdTagBegin, string(uprEvent.Key), base.UdTagEnd)
				return nil, base.ErrorFilterParsingError, additionalErrDesc, releaseFunc, totalFailedCnt
			}
		}
	}

	if !needToProcessBody && shouldSkipKey {
		// This means that the UPR Event coming in is a DCP_MUTATION but is not a JSON document
		// In addition, user did not request filter on Xattribute, nor keys.
		// This is a special case and should be allowed to pass through
		return nil, base.FilterForcePassThrough, additionalErrDesc, releaseFunc, totalFailedCnt
	}
	return body, nil, additionalErrDesc, releaseFunc, totalFailedCnt
}
