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
	"math/rand"

	"github.com/pingcap/pd/pkg/cache"
	"github.com/pingcap/pd/server/core"
)

// Denoising is an option to calculate flow base on the real heartbeats. Should
// only turned off by the simulator and the test.
var Denoising = true

// HotSpotCache is a cache hold hot regions.
type HotSpotCache struct {
	writeFlow *hotPeerCache
	readFlow  *hotPeerCache
}

// NewHotSpotCache creates a new hot spot cache.
func NewHotSpotCache() *HotSpotCache {
	return &HotSpotCache{
		writeFlow: NewHotStoresStats(WriteFlow),
		readFlow:  NewHotStoresStats(ReadFlow),
	}
}

// CheckWrite checks the write status, returns update items.
func (w *HotSpotCache) CheckWrite(region *core.RegionInfo, stats *StoresStats) []*HotPeerStat {
	return w.writeFlow.CheckRegionFlow(region, stats)
}

// CheckRead checks the read status, returns update items.
func (w *HotSpotCache) CheckRead(region *core.RegionInfo, stats *StoresStats) []*HotPeerStat {
	return w.readFlow.CheckRegionFlow(region, stats)
}

// Update updates the cache.
func (w *HotSpotCache) Update(item *HotPeerStat) {
	switch item.Kind {
	case WriteFlow:
		w.writeFlow.Update(item)
	case ReadFlow:
		w.readFlow.Update(item)
	}

	if item.IsNeedDelete() {
		w.incMetrics("remove_item", item.StoreID, item.Kind)
	} else if item.IsNew() {
		w.incMetrics("add_item", item.StoreID, item.Kind)
	} else {
		w.incMetrics("update_item", item.StoreID, item.Kind)
	}
}

// RegionStats returns hot items according to kind
func (w *HotSpotCache) RegionStats(kind FlowKind) map[uint64][]*HotPeerStat {
	var peersOfStore map[uint64]cache.Cache
	switch kind {
	case WriteFlow:
		peersOfStore = w.writeFlow.peersOfStore
	case ReadFlow:
		peersOfStore = w.readFlow.peersOfStore
	}

	res := make(map[uint64][]*HotPeerStat)
	for storeID, peers := range peersOfStore {
		values := peers.Elems()
		stat := make([]*HotPeerStat, len(values))
		res[storeID] = stat
		for i := range values {
			stat[i] = values[i].Value.(*HotPeerStat)
		}
	}
	return res
}

// RandHotRegionFromStore random picks a hot region in specify store.
func (w *HotSpotCache) RandHotRegionFromStore(storeID uint64, kind FlowKind, hotDegree int) *HotPeerStat {
	if stats, ok := w.RegionStats(kind)[storeID]; ok {
		for _, i := range rand.Perm(len(stats)) {
			if stats[i].HotDegree >= hotDegree {
				return stats[i]
			}
		}
	}
	return nil
}

// IsRegionHot checks if the region is hot.
func (w *HotSpotCache) IsRegionHot(region *core.RegionInfo, hotDegree int) bool {
	return w.writeFlow.IsRegionHot(region, hotDegree) ||
		w.readFlow.IsRegionHot(region, hotDegree)
}

// CollectMetrics collect the hot cache metrics
func (w *HotSpotCache) CollectMetrics(stats *StoresStats) {
	w.writeFlow.CollectMetrics(stats, "write")
	w.readFlow.CollectMetrics(stats, "read")
}

func (w *HotSpotCache) incMetrics(name string, storeID uint64, kind FlowKind) {
	store := storeTag(storeID)
	switch kind {
	case WriteFlow:
		hotCacheStatusGauge.WithLabelValues(name, store, "write").Inc()
	case ReadFlow:
		hotCacheStatusGauge.WithLabelValues(name, store, "read").Inc()
	}
}
