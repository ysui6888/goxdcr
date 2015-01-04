// Copyright (c) 2013 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.

package pipeline_svc

import (
	"errors"
	"expvar"
	"fmt"
	"github.com/couchbase/gomemcached"
	mcc "github.com/couchbase/gomemcached/client"
	base "github.com/couchbase/goxdcr/base"
	common "github.com/couchbase/goxdcr/common"
	"github.com/couchbase/goxdcr/log"
	parts "github.com/couchbase/goxdcr/parts"
	"github.com/rcrowley/go-metrics"
	"reflect"
	"runtime/debug"
	"strconv"
	"sync"
	"time"
)

const (
	DOCS_WRITTEN_METRIC    = "docs_written"
	DATA_REPLICATED_METRIC = "data_replicated"
	SIZE_REP_QUEUE_METRIC  = "size_rep_queue"
	DOCS_REP_QUEUE_METRIC  = "docs_rep_queue"
	DOCS_FILTERED_METRIC   = "docs_filtered"
	CHANGES_LEFT_METRIC    = "changes_left"
	DOCS_LATENCY_METRIC    = "wtavg_docs_latency"

	//	TIME_COMMITTING_METRIC = "time_committing"
	//rate
	RATE_REPLICATED	= "rate_replicated"
	BANDWIDTH_USAGE = "bandwidth_usage"

	VB_HIGHSEQNO_PREFIX = "vb_highseqno_"

	OVERVIEW_METRICS_KEY = "Overview"

	//statistics_manager's setting
	SOURCE_NODE_ADDR     = "source_host_addr"
	SOURCE_NODE_USERNAME = "source_host_username"
	SOURCE_NODE_PASSWORD = "source_host_password"
	SAMPLE_SIZE          = "sample_size"
	PUBLISH_INTERVAL     = "publish_interval"
	VB_START_TS          = "v_start_ts"

	//Bucket sequence number statistics
	VBUCKET_SEQNO_STAT_NAME            = "vbucket-seqno"
	VBUCKET_HIGH_SEQNO_STAT_KEY_FORMAT = "vb_%v:high_seqno"
)

const (
	default_sample_size      = 1000
	default_update_interval = 100 * time.Millisecond
)

//StatisticsManager mount the statics collector on the pipeline to collect raw stats
//It does stats correlation and processing on raw stats periodically (controlled by publish_interval)
//, then stores the result in expvar
//The result in expvar can be exposed to outside via different channels - log or to ns_server.
type StatisticsManager struct {
	//a map of registry with the part id as key
	//the aggregated metrics for the pipeline is the entry with key="Overall"
	//this map will be exported to expval, but only
	//the entry with key="Overview" will be reported to ns_server
	registries map[string]metrics.Registry

	//temporary map to keep all the collected start time for data item
	//during this collection interval.
	//At the end of the collection interval, collected starttime and endtime will be correlated
	//to calculate the replication lag. The calculated replication lag will be kept in "Overall"
	//entry in registries.
	//This map will be emptied after the replication lags are calculated to get ready for
	//next collection period
	starttime_map map[string]interface{}

	//temporary map to keep all the collected end time for data item during this collection
	//interval.
	//This map will be emptied after the replication lags are calculated to get ready for
	//next collection period
	endtime_map map[string]interface{}

	//statistics update ticker
	publish_ticker *time.Ticker

	//settings - sample size
	sample_size int
	//settings - statistics update interval
	update_interval time.Duration

	//the channel to communicate finish signal with statistic updater
	finish_ch chan bool
	wait_grp  *sync.WaitGroup

	pipeline common.Pipeline

	logger *log.CommonLogger

	collectors []MetricsCollector

	current_vb_start_ts map[uint16]*base.VBTimestamp
	active_vbs          map[string][]uint16
	bucket_name         string
	bucket_password     string
	kv_mem_clients      map[string]*mcc.Client
}

func NewStatisticsManager(logger_ctx *log.LoggerContext, active_vbs map[string][]uint16, bucket_name string, bucket_password string) *StatisticsManager {
	stats_mgr := &StatisticsManager{registries: make(map[string]metrics.Registry),
		starttime_map:    make(map[string]interface{}),
		finish_ch:        make(chan bool, 1),
		sample_size:      default_sample_size,
		update_interval: default_update_interval,
		logger:           log.NewLogger("StatisticsManager", logger_ctx),
		active_vbs:       active_vbs,
		wait_grp:         &sync.WaitGroup{},
		kv_mem_clients:   make(map[string]*mcc.Client),
		endtime_map:      make(map[string]interface{})}
	stats_mgr.collectors = []MetricsCollector{&xmemCollector{}, &dcpCollector{}, &routerCollector{}}
	return stats_mgr
}

//Statistics of this pipeline which is reported to ReplicationManager
func (stats_mgr *StatisticsManager) Statistics() *expvar.Map {
	expvar_stats_map := stats_mgr.getExpvarMap(stats_mgr.pipeline.Topic())
	overview_map := expvar_stats_map.Get(OVERVIEW_METRICS_KEY)
	if overview_map != nil {
		return overview_map.(*expvar.Map)
	} else {
		return nil
	}

}

//updateStats runs until it get finish signal
//It processes the raw stats and publish the overview stats along with the raw stats to expvar
//It also log the stats to log
func (stats_mgr *StatisticsManager) updateStats(finchan chan bool) error {
	defer stats_mgr.wait_grp.Done()
	for {
		select {
		case <-finchan:
			expvar_stats_map := stats_mgr.getExpvarMap(stats_mgr.pipeline.Topic())
			errlist := expvar_stats_map.Get("Errors")
			expvar_stats_map.Init()
			statusVar := new(expvar.String)
			statusVar.Set(base.Pending)
			expvar_stats_map.Set("Status", statusVar)
			expvar_stats_map.Set("Errors", errlist)
			stats_mgr.logger.Infof("expvar=%v\n", stats_mgr.formatStatsForLog())
			return nil
		case <-stats_mgr.publish_ticker.C:
			stats_mgr.logger.Debugf("%v: Publishing the statistics for %v to expvar", time.Now(), stats_mgr.pipeline.Topic())
			stats_mgr.processRawStats()
			if stats_mgr.logger.GetLogLevel() >= log.LogLevelInfo {
				stats_mgr.logger.Info(stats_mgr.formatStatsForLog())
			}
		}
	}
	return nil
}

func (stats_mgr *StatisticsManager) formatStatsForLog() string {
	expvar_stats_map := stats_mgr.getExpvarMap(stats_mgr.pipeline.Topic())
	return fmt.Sprintf("Stats for pipeline %v\n %v\n", stats_mgr.pipeline.Topic(), expvar_stats_map.String())
}

//process the raw stats, aggregate them into overview registry
//expose the raw stats and overview stats to expvar
func (stats_mgr *StatisticsManager) processRawStats() {
	oldSample := stats_mgr.getOverviewRegistry()
	stats_mgr.initOverviewRegistry()
	expvar_stats_map := stats_mgr.getExpvarMap(stats_mgr.pipeline.Topic())

	stats_mgr.processTimeSample()
	for registry_name, registry := range stats_mgr.registries {
		if registry_name != OVERVIEW_METRICS_KEY {
			map_for_registry := new(expvar.Map).Init()

			orig_registry := expvar_stats_map.Get(registry_name)
			registry.Each(func(name string, i interface{}) {
				stats_mgr.publishMetricToMap(map_for_registry, name, i, true)
				switch m := i.(type) {
				case metrics.Counter:
					if orig_registry != nil {
						orig_val, _ := strconv.ParseInt(orig_registry.(*expvar.Map).Get(name).String(), 10, 64)
						if m.Count() < orig_val {
							panic(fmt.Sprintf("counter %v goes backward\n", name))
						}
					}
					metric_overview := stats_mgr.getOverviewRegistry().Get(name)
					if metric_overview != nil {
						metric_overview.(metrics.Counter).Inc(m.Count())
					}

				}
			})
			expvar_stats_map.Set(registry_name, map_for_registry)
		}
	}

	map_for_overview := new(expvar.Map).Init()

	//publish all the metrics in overview registry
	stats_mgr.getOverviewRegistry().Each(func(name string, i interface{}) {
		stats_mgr.publishMetricToMap(map_for_overview, name, i, false)
	})

	//calculate the publish additional metrics
	stats_mgr.processCalculatedStats(oldSample, map_for_overview)

	expvar_stats_map.Set(OVERVIEW_METRICS_KEY, map_for_overview)
}

func (stats_mgr *StatisticsManager) processCalculatedStats(oldSample metrics.Registry, overview_expvar_map *expvar.Map) {

	//calculate changes_left
	docs_written := stats_mgr.getOverviewRegistry().Get(DOCS_WRITTEN_METRIC).(metrics.Counter).Count()
	changes_left_val, err := stats_mgr.calculateChangesLeft(docs_written)
	if err == nil {
		changes_left_var := new(expvar.Int)
		changes_left_var.Set(changes_left_val)
		overview_expvar_map.Set(CHANGES_LEFT_METRIC, changes_left_var)
	} else {
		stats_mgr.logger.Errorf("Failed to calculate changes_left - %v\n", err)
	}
	
	//calculate rate_replication
	docs_written_old := oldSample.Get(DOCS_WRITTEN_METRIC).(metrics.Counter).Count()
	interval_in_sec := stats_mgr.update_interval.Seconds()
	rate_replicated := float64(docs_written - docs_written_old)/interval_in_sec
	rate_replicated_var := new(expvar.Float)
	rate_replicated_var.Set(rate_replicated)
	overview_expvar_map.Set(RATE_REPLICATED, rate_replicated_var)
	
	//calculate bandwidth_usage
	data_replicated_old := oldSample.Get(DATA_REPLICATED_METRIC).(metrics.Counter).Count()
	data_replicated := stats_mgr.getOverviewRegistry().Get(DATA_REPLICATED_METRIC).(metrics.Counter).Count()
	bandwidth_usage := float64(data_replicated - data_replicated_old)/interval_in_sec
	bandwidth_usage_var := new(expvar.Float)
	bandwidth_usage_var.Set(bandwidth_usage)
	overview_expvar_map.Set(BANDWIDTH_USAGE, bandwidth_usage_var)
}

func (stats_mgr *StatisticsManager) calculateChangesLeft(docs_written int64) (int64, error) {
	total_doc, err := stats_mgr.calculateTotalChanges()
	if err != nil {
		return 0, err
	}
	changes_left := total_doc - docs_written
	stats_mgr.logger.Debugf("total_doc=%v, docs_written=%v, changes_left=%v\n", total_doc, docs_written, changes_left)
	return changes_left, nil
}

func (stats_mgr *StatisticsManager) calculateTotalChanges() (int64, error) {
	var total_doc uint64 = 0
	for serverAddr, vbnos := range stats_mgr.active_vbs {
		highseqno_map, err := stats_mgr.getHighSeqNos(serverAddr, vbnos)
		for _, vbno := range vbnos {
			ts := stats_mgr.current_vb_start_ts[vbno]
			current_vb_highseqno := highseqno_map[vbno]
			if err != nil {
				return 0, err
			}
			total_doc = total_doc + current_vb_highseqno - ts.Seqno
		}
	}
	return int64(total_doc), nil
}
func (stats_mgr *StatisticsManager) getHighSeqNos(serverAddr string, vbnos []uint16) (map[uint16]uint64, error) {
	highseqno_map := make(map[uint16]uint64)
	conn := stats_mgr.kv_mem_clients[serverAddr]
	if conn == nil {
		return nil, errors.New("connection for serverAddr is not initialized")
	}

	stats_map, err := conn.StatsMap(VBUCKET_SEQNO_STAT_NAME)
	if err != nil {
		return nil, err
	}

	for _, vbno := range vbnos {
		stats_key := fmt.Sprintf(VBUCKET_HIGH_SEQNO_STAT_KEY_FORMAT, vbno)
		highseqnostr := stats_map[stats_key]
		highseqno, err := strconv.ParseUint(highseqnostr, 10, 64)
		if err != nil {
			return nil, err
		}
		highseqno_map[vbno] = highseqno
	}

	return highseqno_map, nil
}

func (stats_mgr *StatisticsManager) getExpvarMap(name string) *expvar.Map {
	pipeline_map := expvar.Get(name)

	return pipeline_map.(*expvar.Map)
}

func (stats_mgr *StatisticsManager) getOverviewRegistry() metrics.Registry {
	return stats_mgr.registries[OVERVIEW_METRICS_KEY]
}

func (stats_mgr *StatisticsManager) publishMetricToMap(expvar_map *expvar.Map, name string, i interface{}, includeDetails bool) {
	switch m := i.(type) {
	case metrics.Counter:
		expvar_map.Set(name, expvar.Func(func() interface{} {
			return m.Count()
		}))
	case metrics.Histogram:
		if includeDetails {
			metrics_map := new(expvar.Map).Init()
			mean := new(expvar.Float)
			mean.Set(m.Mean())
			metrics_map.Set("mean", mean)
			max := new(expvar.Int)
			max.Set(m.Max())
			metrics_map.Set("max", max)
			min := new(expvar.Int)
			min.Set(m.Min())
			metrics_map.Set("min", min)
			count := new(expvar.Int)
			count.Set(m.Count())
			metrics_map.Set("count", count)
			expvar_map.Set(name, metrics_map)
		} else {
			mean := new(expvar.Float)
			mean.Set(m.Mean())
			expvar_map.Set(name, mean)
		}

	}
}

func (stats_mgr *StatisticsManager) processTimeSample() {
	stats_mgr.logger.Info("Process Time Sample...")
	time_committing := stats_mgr.getOverviewRegistry().GetOrRegister(DOCS_LATENCY_METRIC, metrics.NewHistogram(metrics.NewUniformSample(stats_mgr.sample_size))).(metrics.Histogram)
	time_committing.Clear()
	sample := time_committing.Sample()
	for name, starttime := range stats_mgr.starttime_map {
		endtime := stats_mgr.endtime_map[name]
		if endtime != nil {
			rep_duration := endtime.(time.Time).Sub(starttime.(time.Time))
			//in millisecond
			sample.Update(rep_duration.Nanoseconds()/1000000)
		}
	}

	//clear both starttime_registry and endtime_registry
	stats_mgr.starttime_map = make(map[string]interface{})
	stats_mgr.endtime_map = make(map[string]interface{})
}

func (stats_mgr *StatisticsManager) getOrCreateRegistry(name string) metrics.Registry {
	registry := stats_mgr.registries[name]
	if registry == nil {
		registry = metrics.NewRegistry()
		stats_mgr.registries[name] = registry
	}
	return registry
}

func (stats_mgr *StatisticsManager) Attach(pipeline common.Pipeline) error {
	stats_mgr.pipeline = pipeline

	//mount collectors with pipeline
	for _, collector := range stats_mgr.collectors {
		collector.Mount(pipeline, stats_mgr)
	}

	//register the aggregation metrics for the pipeline
	stats_mgr.initOverviewRegistry()
	stats_mgr.logger.Infof("StatisticsManager is started for pipeline %v", stats_mgr.pipeline.Topic)

	//publish the statistics to expvar
	expvar_map := expvar.Get(stats_mgr.pipeline.Topic())
	if expvar_map == nil {
		expvar.NewMap(stats_mgr.pipeline.Topic())
	} 
	return nil
}

func (stats_mgr *StatisticsManager) initOverviewRegistry() {
	overview_registry := metrics.NewRegistry()
	stats_mgr.registries[OVERVIEW_METRICS_KEY] = overview_registry
	overview_registry.Register(DOCS_WRITTEN_METRIC, metrics.NewCounter())
	overview_registry.Register(DATA_REPLICATED_METRIC, metrics.NewCounter())
	overview_registry.Register(DOCS_FILTERED_METRIC, metrics.NewCounter())
}

func (stats_mgr *StatisticsManager) Start(settings map[string]interface{}) error {

	//initialize connection
	stats_mgr.initConnection()

	if _, ok := settings[PUBLISH_INTERVAL]; ok {
		stats_mgr.update_interval = settings[PUBLISH_INTERVAL].(time.Duration)
	}
	stats_mgr.logger.Infof("StatisticsManager Starts: update_interval=%v\n", stats_mgr.update_interval)
	debug.PrintStack()
	stats_mgr.publish_ticker = time.NewTicker(stats_mgr.update_interval)

	if _, ok := settings[VB_START_TS]; ok {
		stats_mgr.current_vb_start_ts = settings[VB_START_TS].(map[uint16]*base.VBTimestamp)
	}

	//publishing status to expvar
	expvar_stats_map := stats_mgr.getExpvarMap(stats_mgr.pipeline.Topic())
	statusVar := new(expvar.String)
	statusVar.Set(base.Replicating)
	expvar_stats_map.Set("Status", statusVar)
	stats_mgr.wait_grp.Add(1)
	go stats_mgr.updateStats(stats_mgr.finish_ch)

	return nil
}

func (stats_mgr *StatisticsManager) Stop() error {
	stats_mgr.logger.Infof("StatisticsManager Stopping...")
	stats_mgr.finish_ch <- true

	//close the connections
	for _, client := range stats_mgr.kv_mem_clients {
		client.Close()
	}

	stats_mgr.wait_grp.Wait()
	stats_mgr.logger.Infof("StatisticsManager Stopped")

	return nil
}

func (stats_mgr *StatisticsManager) initConnection() error {
	for serverAddr, _ := range stats_mgr.active_vbs {
		conn, err := base.NewConn(serverAddr, stats_mgr.bucket_name, stats_mgr.bucket_password)
		if err != nil {
			return err
		}
		stats_mgr.kv_mem_clients[serverAddr] = conn
	}

	return nil
}

type MetricsCollector interface {
	Mount(pipeline common.Pipeline, stats_mgr *StatisticsManager) error
}

//metrics collector for XMemNozzle
type xmemCollector struct {
	stats_mgr *StatisticsManager
}

func (xmem_collector *xmemCollector) Mount(pipeline common.Pipeline, stats_mgr *StatisticsManager) error {
	xmem_collector.stats_mgr = stats_mgr
	xmem_parts := pipeline.Targets()
	for _, part := range xmem_parts {
		registry := stats_mgr.getOrCreateRegistry(part.Id())
		registry.Register(SIZE_REP_QUEUE_METRIC, metrics.NewHistogram(metrics.NewUniformSample(stats_mgr.sample_size)))
		registry.Register(DOCS_REP_QUEUE_METRIC, metrics.NewHistogram(metrics.NewUniformSample(stats_mgr.sample_size)))
		registry.Register(DOCS_WRITTEN_METRIC, metrics.NewCounter())
		registry.Register(DATA_REPLICATED_METRIC, metrics.NewCounter())
		part.RegisterComponentEventListener(common.DataSent, xmem_collector)
		part.RegisterComponentEventListener(common.DataReceived, xmem_collector)

	}
	return nil
}

func (xmem_collector *xmemCollector) OnEvent(eventType common.ComponentEventType,
	item interface{},
	component common.Component,
	derivedItems []interface{},
	otherInfos map[string]interface{}) {
	if eventType == common.DataReceived {
		xmem_collector.stats_mgr.logger.Debugf("Received a DataReceived event from %v", reflect.TypeOf(component))
		queue_size := otherInfos[parts.XMEM_STATS_QUEUE_SIZE].(int)
		queue_size_bytes := otherInfos[parts.XMEM_STATS_QUEUE_SIZE_BYTES].(int)
		registry := xmem_collector.stats_mgr.registries[component.Id()]
		registry.Get(DOCS_REP_QUEUE_METRIC).(metrics.Histogram).Sample().Update(int64(queue_size))
		registry.Get(SIZE_REP_QUEUE_METRIC).(metrics.Histogram).Sample().Update(int64(queue_size_bytes))
	} else if eventType == common.DataSent {
		endTime := time.Now()
		size := item.(*gomemcached.MCRequest).Size()
		seqno := otherInfos[parts.XMEM_EVENT_ADDI_SEQNO].(uint64)
		registry := xmem_collector.stats_mgr.registries[component.Id()]
		registry.Get(DOCS_WRITTEN_METRIC).(metrics.Counter).Inc(1)
		registry.Get(DATA_REPLICATED_METRIC).(metrics.Counter).Inc(int64(size))

		xmem_collector.stats_mgr.endtime_map[fmt.Sprintf("%v", seqno)] = endTime
	}
}

//metrics collector for DcpNozzle
type dcpCollector struct {
	stats_mgr *StatisticsManager
}

func (dcp_collector *dcpCollector) Mount(pipeline common.Pipeline, stats_mgr *StatisticsManager) error {
	dcp_collector.stats_mgr = stats_mgr
	dcp_parts := pipeline.Sources()
	for _, dcp_part := range dcp_parts {
		stats_mgr.getOrCreateRegistry(dcp_part.Id())
		dcp_part.RegisterComponentEventListener(common.DataReceived, dcp_collector)
	}
	return nil
}

func (dcp_collector *dcpCollector) OnEvent(eventType common.ComponentEventType,
	item interface{},
	component common.Component,
	derivedItems []interface{},
	otherInfos map[string]interface{}) {
	if eventType == common.DataReceived {
		dcp_collector.stats_mgr.logger.Debugf("Received a DataReceived event from %v", reflect.TypeOf(component))
		startTime := time.Now()
		seqno := item.(*mcc.UprEvent).Seqno
		dcp_collector.stats_mgr.starttime_map[fmt.Sprintf("%v", seqno)] = startTime
	}
}

//metrics collector for Router
type routerCollector struct {
	stats_mgr *StatisticsManager
}

func (r_collector *routerCollector) Mount(pipeline common.Pipeline, stats_mgr *StatisticsManager) error {
	r_collector.stats_mgr = stats_mgr
	dcp_parts := pipeline.Sources()
	for _, dcp_part := range dcp_parts {
		//get connector
		conn := dcp_part.Connector()
		registry_router := stats_mgr.getOrCreateRegistry(conn.Id())
		registry_router.Register(DOCS_FILTERED_METRIC, metrics.NewCounter())
		conn.RegisterComponentEventListener(common.DataFiltered, r_collector)
	}
	return nil
}

func (l_collector *routerCollector) OnEvent(eventType common.ComponentEventType,
	item interface{},
	component common.Component,
	derivedItems []interface{},
	otherInfos map[string]interface{}) {
	if eventType == common.DataFiltered {
		seqno := item.(*mcc.UprEvent).Seqno
		l_collector.stats_mgr.logger.Debugf("Received a DataFiltered event for %v", seqno)
		registry := l_collector.stats_mgr.registries[component.Id()]
		registry.Get(DOCS_FILTERED_METRIC).(metrics.Counter).Inc(1)
	}
}