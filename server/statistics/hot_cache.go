// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package statistics

import (
	"fmt"
	"math/rand"

	"github.com/pingcap/pd/pkg/cache"
	"github.com/pingcap/pd/server/core"
)

// Denoising is an option to calculate flow base on the real heartbeats. Should
// only turned off by the simulator and the test.
var Denoising = true

const (
	// RegionHeartBeatReportInterval is the heartbeat report interval of a region.
	RegionHeartBeatReportInterval = 60
	// StoreHeartBeatReportInterval is the heartbeat report interval of a store.
	StoreHeartBeatReportInterval = 10

	statCacheMaxLen            = 1000
	storeStatCacheMaxLen       = 200
	hotWriteRegionMinFlowRate  = 16 * 1024
	hotReadRegionMinFlowRate   = 128 * 1024
	minHotRegionReportInterval = 3
	hotRegionAntiCount         = 1
)

// HotSpotCache is a cache hold hot regions.
type HotSpotCache struct {
	writeFlow *hotStoresStats
	readFlow  *hotStoresStats
}

// NewHotSpotCache creates a new hot spot cache.
func NewHotSpotCache() *HotSpotCache {
	return &HotSpotCache{
		writeFlow: NewHotStoresStats(),
		readFlow:  NewHotStoresStats(),
	}
}

// CheckWrite checks the write status, returns update items.
func (w *HotSpotCache) CheckWrite(region *core.RegionInfo, stats *StoresStats) []*HotSpotPeerStat {
	var updateItems []*HotSpotPeerStat
	hotStatGenerators := w.writeFlow.CheckRegionFlow(region, WriteFlow)
	for _, hotGen := range hotStatGenerators {
		item := hotGen.GenHotSpotPeerStats(stats)
		if item != nil {
			updateItems = append(updateItems, item)
		}
	}
	return updateItems
}

// CheckRead checks the read status, returns update items.
func (w *HotSpotCache) CheckRead(region *core.RegionInfo, stats *StoresStats) []*HotSpotPeerStat {
	var updateItems []*HotSpotPeerStat
	hotStatGenerators := w.readFlow.CheckRegionFlow(region, ReadFlow)
	for _, hotGen := range hotStatGenerators {
		item := hotGen.GenHotSpotPeerStats(stats)
		if item != nil {
			updateItems = append(updateItems, item)
		}
	}
	return updateItems
}

// Update updates the cache.
func (w *HotSpotCache) Update(item *HotSpotPeerStat) {
	var stats *hotStoresStats
	switch item.Kind {
	case WriteFlow:
		stats = w.writeFlow
	case ReadFlow:
		stats = w.readFlow
	}
	stats.Update(item)
	if item.IsNeedDelete() {
		w.incMetrics("remove_item", item.StoreID, item.Kind)
	} else if item.IsNew() {
		w.incMetrics("add_item", item.StoreID, item.Kind)
	} else {
		w.incMetrics("update_item", item.StoreID, item.Kind)
	}
}

// RegionStats returns hot items according to kind
func (w *HotSpotCache) RegionStats(kind FlowKind) map[uint64][]*HotSpotPeerStat {
	var flowMap map[uint64]cache.Cache
	switch kind {
	case WriteFlow:
		flowMap = w.writeFlow.hotStoreStats
	case ReadFlow:
		flowMap = w.readFlow.hotStoreStats
	}
	res := make(map[uint64][]*HotSpotPeerStat)
	for storeID, elements := range flowMap {
		values := elements.Elems()
		stat, ok := res[storeID]
		if !ok {
			stat = make([]*HotSpotPeerStat, len(values))
			res[storeID] = stat
		}
		for i := range values {
			stat[i] = values[i].Value.(*HotSpotPeerStat)
		}
	}
	return res
}

// RandHotRegionFromStore random picks a hot region in specify store.
func (w *HotSpotCache) RandHotRegionFromStore(storeID uint64, kind FlowKind, hotThreshold int) *HotSpotPeerStat {
	stats, ok := w.RegionStats(kind)[storeID]
	if !ok {
		return nil
	}
	for _, i := range rand.Perm(len(stats)) {
		if stats[i].HotDegree >= hotThreshold {
			return stats[i]
		}
	}
	return nil
}

// IsRegionHot checks if the region is hot.
func (w *HotSpotCache) IsRegionHot(region *core.RegionInfo, hotThreshold int) bool {
	stats := w.writeFlow
	if stats.isRegionHotWithAnyPeers(region, hotThreshold) {
		return true
	}
	stats = w.readFlow
	return stats.isRegionHotWithPeer(region, region.GetLeader(), hotThreshold)
}

// CollectMetrics collect the hot cache metrics
func (w *HotSpotCache) CollectMetrics(stats *StoresStats) {
	for storeID, flowStats := range w.writeFlow.hotStoreStats {
		storeTag := fmt.Sprintf("store-%d", storeID)
		threshold := calculateWriteHotThresholdWithStore(stats, storeID)
		hotCacheStatusGauge.WithLabelValues("total_length", storeTag, "write").Set(float64(flowStats.Len()))
		hotCacheStatusGauge.WithLabelValues("hotThreshold", storeTag, "write").Set(float64(threshold))
	}

	for storeID, flowStats := range w.readFlow.hotStoreStats {
		storeTag := fmt.Sprintf("store-%d", storeID)
		threshold := calculateReadHotThresholdWithStore(stats, storeID)
		hotCacheStatusGauge.WithLabelValues("total_length", storeTag, "read").Set(float64(flowStats.Len()))
		hotCacheStatusGauge.WithLabelValues("hotThreshold", storeTag, "read").Set(float64(threshold))
	}
}

func (w *HotSpotCache) incMetrics(name string, storeID uint64, kind FlowKind) {
	storeTag := fmt.Sprintf("store-%d", storeID)
	switch kind {
	case WriteFlow:
		hotCacheStatusGauge.WithLabelValues(name, storeTag, "write").Inc()
	case ReadFlow:
		hotCacheStatusGauge.WithLabelValues(name, storeTag, "read").Inc()
	}
}

// Utils
func calculateWriteHotThresholdWithStore(stats *StoresStats, storeID uint64) uint64 {
	writeBytes, _ := stats.GetStoreBytesRate(storeID)
	divisor := float64(storeStatCacheMaxLen) * 2
	hotRegionThreshold := uint64(float64(writeBytes) / divisor)

	if hotRegionThreshold < hotWriteRegionMinFlowRate {
		hotRegionThreshold = hotWriteRegionMinFlowRate
	}
	return hotRegionThreshold
}

func calculateReadHotThresholdWithStore(stats *StoresStats, storeID uint64) uint64 {
	_, readBytes := stats.GetStoreBytesRate(storeID)
	divisor := float64(storeStatCacheMaxLen) * 2
	hotRegionThreshold := uint64(float64(readBytes) / divisor)

	if hotRegionThreshold < hotReadRegionMinFlowRate {
		hotRegionThreshold = hotReadRegionMinFlowRate
	}
	return hotRegionThreshold
}
