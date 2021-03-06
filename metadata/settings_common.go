// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package metadata

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/couchbase/goxdcr/base"
	"github.com/couchbase/goxdcr/log"
	"reflect"
	"strconv"
)

var logger_sc *log.CommonLogger = log.NewLogger("SettingsCommon", log.DefaultLoggerContext)

type SettingsConfig struct {
	defaultValue interface{}
	*Range
}

type Range struct {
	MinValue int
	MaxValue int
}

// ConfigMapRetriever retrieves default config map for settings
type ConfigMapRetriever func() map[string]*SettingsConfig

type Settings struct {
	// key - name of setting
	// value - value of setting
	// value could be a primitive type, int, bool, string, etc., or a user defined type like LogLevel
	Values map[string]interface{} `json:"values"`

	// pointer to function that retrieves config map
	// not exposed and not saved to metakv
	// WARNING: parent class that embeds Settings MUST set this pointer
	configMapRetriever ConfigMapRetriever
}

func EmptySettings(configMapRetriever ConfigMapRetriever) *Settings {
	return &Settings{Values: make(map[string]interface{}),
		configMapRetriever: configMapRetriever}
}

func DefaultSettings(configMapRetriever ConfigMapRetriever) *Settings {
	settings := EmptySettings(configMapRetriever)
	for settingsKey, settingsConfig := range settings.configMapRetriever() {
		settings.Values[settingsKey] = settingsConfig.defaultValue
	}
	return settings
}

func (s *Settings) Equals(s2 *Settings) bool {
	if s == s2 {
		// this also covers the case where s = nil and s2 = nil
		return true
	}
	if (s == nil && s2 != nil) || (s != nil && s2 == nil) {
		return false
	}

	// when we get here, s != nil and s2 != nil
	if len(s.Values) != len(s2.Values) {
		return false
	}

	for key, value := range s.Values {
		value2, ok := s2.Values[key]
		if !ok {
			return false
		}
		// use != operator on value, which should be a primitive type or enum type
		if value != value2 {
			return false
		}
	}

	// no way to compare configMapRetriever. skip it

	return true
}

func (s *Settings) UpdateSettingsFromMap(settingsMap map[string]interface{}) (changedSettingsMap ReplicationSettingsMap, errorMap map[string]error) {
	changedSettingsMap = make(ReplicationSettingsMap)
	errorMap = make(map[string]error)
	configMap := s.configMapRetriever()

	for settingKey, settingValue := range settingsMap {
		settingConfig, ok := configMap[settingKey]
		if !ok {
			// not a valid settings key
			errorMap[settingKey] = base.ErrorInvalidSettingsKey
			continue
		}

		expectedType := reflect.TypeOf(settingConfig.defaultValue)
		actualType := reflect.TypeOf(settingValue)
		if expectedType != actualType {
			// type of the value does not match
			errorMap[settingKey] = fmt.Errorf("Invalid type of value in map for %v. expected=%v, actual=%v", settingKey, expectedType, actualType)
			continue
		}

		oldSettingValue, ok := s.Values[settingKey]
		if !ok || settingValue != oldSettingValue {
			s.Values[settingKey] = settingValue
			changedSettingsMap[settingKey] = settingValue
		}
	}
	return
}

func (s *Settings) ToMap() map[string]interface{} {
	settingsMap := make(map[string]interface{})
	for key, value := range s.Values {
		settingsMap[key] = value
	}
	return settingsMap
}

func (s *Settings) Clone() *Settings {
	clone := EmptySettings(s.configMapRetriever)
	for key, value := range s.Values {
		clone.Values[key] = value
	}
	return clone
}

// after settings is loaded from metakv and unmarshalled, it needs some extra processing
func (s *Settings) PostProcessAfterUnmarshalling(configMapRetriever ConfigMapRetriever) {
	// this is super critical
	s.configMapRetriever = configMapRetriever
	s.HandleDataTypeConversionAfterUnmarshalling()
}

// since setting value is defined as interface{}, the actual data type of setting value may change
// after marshalling and unmarshalling
// for example, an "int" type value becomes "float64" after marshalling and unmarshalling
// it is necessary to convert such data type back
func (s *Settings) HandleDataTypeConversionAfterUnmarshalling() {
	errorsMap := make(base.ErrorMap)
	configMap := s.configMapRetriever()

	for settingKey, settingValue := range s.Values {
		settingConfig, ok := configMap[settingKey]
		if !ok {
			// should never get here
			errorsMap[settingKey] = errors.New("not a valid setting")
			continue
		}
		valueTypeKind := reflect.TypeOf(settingConfig.defaultValue).Kind()
		switch valueTypeKind {
		case reflect.Int:
			intValue, err := handleIntTypeConversion(settingValue)
			if err != nil {
				// should never get here
				errorsMap[settingKey] = err
				continue
			}
			s.Values[settingKey] = intValue
		}
	}

	if len(errorsMap) > 0 {
		logger_sc.Warnf("Settings unmarshalled from metakv has the following issues: %v\n settings=%v\n", errorsMap, s)

		// remove problematic key/value to avoid problems down the road
		// default values will be used for the removed keys
		for problematicSettingKey, _ := range errorsMap {
			delete(s.Values, problematicSettingKey)
		}
	}
}

// after upgrade, settings that did not exist in before-upgrade version do not exist in s.Values
// add these settings to s.Values with default values
func (s *Settings) HandleUpgrade() {
	for settingsKey, settingsConfig := range s.configMapRetriever() {
		if _, ok := s.Values[settingsKey]; !ok {
			s.Values[settingsKey] = settingsConfig.defaultValue
		}
	}
}

func (s *Settings) String() string {
	if s == nil {
		return "nil"
	}

	var buffer bytes.Buffer
	first := true
	for key, value := range s.Values {
		if first {
			first = false
		} else {
			buffer.WriteString(", ")
		}
		buffer.WriteString(key)
		buffer.WriteByte(':')
		buffer.WriteString(fmt.Sprintf("%v", value))
	}
	return buffer.String()
}

// get the value for the specified key
// if value is not found in Values map, return default value
func (s *Settings) GetSettingValueOrDefaultValue(key string) (interface{}, error) {
	settingConfig, ok := s.configMapRetriever()[key]
	if !ok {
		return nil, fmt.Errorf("%v is not a valid replication setting", key)
	}

	value, ok := s.Values[key]
	if !ok {
		// use default value if not found in Values map
		return settingConfig.defaultValue, nil
	}

	return value, nil
}

// for ease of use, this method does not return error
// caller needs to ensure that it is called with a valid setting key that points to an integer setting
func (s *Settings) GetIntSettingValue(key string) int {
	value, _ := s.GetSettingValueOrDefaultValue(key)
	return value.(int)
}

// for ease of use, this method does not return error
// caller needs to ensure that it is called with a valid setting key that points to a string setting
func (s *Settings) GetStringSettingValue(key string) string {
	value, _ := s.GetSettingValueOrDefaultValue(key)
	return value.(string)
}

// for ease of use, this method does not return error
// caller needs to ensure that it is called with a valid setting key that points to a bool setting
func (s *Settings) GetBoolSettingValue(key string) bool {
	value, _ := s.GetSettingValueOrDefaultValue(key)
	return value.(bool)
}

func handleIntTypeConversion(settingValue interface{}) (int, error) {
	// if an integer type setting is unmarshalled from metakv, the value would be float64 type
	floatValue, ok := settingValue.(float64)
	if ok {
		return int(floatValue), nil
	}

	return 0, fmt.Errorf("value is of unexpected data type, %v", reflect.TypeOf(settingValue))
}

// input settingsMap contains keys that may or may not belong to Settings
// get keys specific to Settings and copy the corresponding entries to newSettingsMap
func ValidateSettingsKey(settingsMap map[string]interface{}, configMap map[string]*SettingsConfig) (newSettingsMap map[string]interface{}) {
	newSettingsMap = make(map[string]interface{})

	for key, val := range settingsMap {
		if _, ok := configMap[key]; ok {
			newSettingsMap[key] = val
		}
	}
	return
}

func ValidateAndConvertSettingsValue(key, value string, configMap map[string]*SettingsConfig) (interface{}, error) {
	settingConfig, ok := configMap[key]
	if !ok {
		return nil, base.ErrorInvalidSettingsKey
	}

	valueTypeKind := reflect.TypeOf(settingConfig.defaultValue).Kind()
	switch valueTypeKind {
	case reflect.Int:
		return validateAndConvertIntValue(value, settingConfig)
	case reflect.Bool:
		return validateAndConvertBoolValue(value, settingConfig)
	default:
		// no validation for other types
		return value, nil
	}
}

func validateAndConvertIntValue(value string, settingConfig *SettingsConfig) (convertedValue interface{}, err error) {
	convertedValue, err = strconv.ParseInt(value, base.ParseIntBase, base.ParseIntBitSize)
	if err != nil {
		err = base.IncorrectValueTypeError("an integer")
		return
	}

	convertedValue = int(convertedValue.(int64))
	err = RangeCheck(convertedValue.(int), settingConfig)
	return
}

func validateAndConvertBoolValue(value string, settingConfig *SettingsConfig) (convertedValue interface{}, err error) {
	convertedValue, err = strconv.ParseBool(value)
	if err != nil {
		err = base.IncorrectValueTypeError("a boolean")
		return
	}
	return
}

// range check for int parameters
func RangeCheck(intValue int, settingsConfig *SettingsConfig) error {
	if settingsConfig.Range != nil {
		if intValue < settingsConfig.Range.MinValue || intValue > settingsConfig.Range.MaxValue {
			return base.InvalidValueError("an integer", settingsConfig.Range.MinValue, settingsConfig.Range.MaxValue)
		}
	}
	return nil
}

// If a feature is enabled, and it only works with Enterprise, return an error
func enterpriseOnlyFeature(convertedValue, defaultValue interface{}, isEnterprise bool) error {
	if convertedValue != defaultValue {
		if !isEnterprise {
			return errors.New("The value can be specified only in enterprise edition")
		}
	}
	return nil
}

// If a feature is enabled, and it only works with non-CAPI, return an error
func nonCAPIOnlyFeature(convertedValue, defaultValue interface{}, isCapi bool) error {
	if convertedValue != defaultValue {
		if isCapi {
			return errors.New("The value can not be specified for CAPI replication")
		}
	}
	return nil
}
