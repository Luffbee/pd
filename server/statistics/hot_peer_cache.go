package statistics

import (
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/pkg/cache"
	"github.com/pingcap/pd/server/core"
)

const (
	cacheMaxLen     = 1000
	hotPeerMaxCount = 400

	rollingWindowsSize = 5

	hotWriteRegionMinFlowRate = 16 * 1024
	hotReadRegionMinFlowRate  = 128 * 1024

	hotRegionReportMinInterval = 3

	hotRegionAntiCount = 1
)

// hotPeerCache saves the hotspot peer's statistics.
type hotPeerCache struct {
	kind           FlowKind
	peersOfStore   map[uint64]cache.Cache         // storeID -> hot peers
	storesOfRegion map[uint64]map[uint64]struct{} // regionID -> storeIDs
}

// NewHotStoresStats creates a HotStoresStats
func NewHotStoresStats(kind FlowKind) *hotPeerCache {
	return &hotPeerCache{
		kind:           kind,
		peersOfStore:   make(map[uint64]cache.Cache),
		storesOfRegion: make(map[uint64]map[uint64]struct{}),
	}
}

// Update updates the items in statistics.
func (f *hotPeerCache) Update(item *HotPeerStat) {
	if item.IsNeedDelete() {
		if peers, ok := f.peersOfStore[item.StoreID]; ok {
			peers.Remove(item.RegionID)
		}

		if stores, ok := f.storesOfRegion[item.RegionID]; ok {
			delete(stores, item.StoreID)
		}
	} else {
		peers, ok := f.peersOfStore[item.StoreID]
		if !ok {
			peers = cache.NewCache(cacheMaxLen, cache.TwoQueueCache)
			f.peersOfStore[item.StoreID] = peers
		}
		peers.Put(item.RegionID, item)

		stores, ok := f.storesOfRegion[item.RegionID]
		if !ok {
			stores = make(map[uint64]struct{})
			f.storesOfRegion[item.RegionID] = stores
		}
		stores[item.StoreID] = struct{}{}
	}
}

// CheckRegionFlow checks the flow information of region.
func (f *hotPeerCache) CheckRegionFlow(region *core.RegionInfo, stats *StoresStats) (ret []*HotPeerStat) {
	storeIDs := f.getAllStoreIDs(region)

	bytesFlow := f.getBytesFlow(region)
	keysFlow := f.getKeysFlow(region)

	bytesPerSecInit := uint64(float64(bytesFlow) / float64(RegionHeartBeatReportInterval))
	keysPerSecInit := uint64(float64(keysFlow) / float64(RegionHeartBeatReportInterval))

	for storeID := range storeIDs {
		bytesPerSec := bytesPerSecInit
		keysPerSec := keysPerSecInit
		isExpired := f.isRegionExpired(region, storeID)
		oldItem := f.getOldHotPeerStat(region.GetID(), storeID)

		// This is used for the simulator.
		if oldItem != nil && Denoising {
			interval := time.Since(oldItem.LastUpdateTime).Seconds()
			// ignore if report too fast
			if interval < hotRegionReportMinInterval && !isExpired {
				continue
			}
			bytesPerSec = uint64(float64(bytesFlow) / interval)
			keysPerSec = uint64(float64(keysFlow) / interval)
		}

		newItem := &HotPeerStat{
			StoreID:        storeID,
			RegionID:       region.GetID(),
			Kind:           f.kind,
			BytesRate:      bytesPerSec,
			KeysRate:       keysPerSec,
			LastUpdateTime: time.Now(),
			Version:        region.GetMeta().GetRegionEpoch().GetVersion(),
			needDelete:     isExpired,
			isLeader:       region.GetLeader().GetStoreId() == storeID,
		}

		hotThreshold := f.calcHotThreshold(stats, storeID)
		newItem = updateHotPeerStat(newItem, oldItem, bytesPerSec, hotThreshold)
		if newItem != nil {
			ret = append(ret, newItem)
		}
	}

	return ret
}

func (f *hotPeerCache) IsRegionHot(region *core.RegionInfo, hotDegree int) bool {
	switch f.kind {
	case WriteFlow:
		return f.isRegionHotWithAnyPeers(region, hotDegree)
	case ReadFlow:
		return f.isRegionHotWithPeer(region, region.GetLeader(), hotDegree)
	}
	return false
}

func (f *hotPeerCache) CollectMetrics(stats *StoresStats, typ string) {
	for storeID, peers := range f.peersOfStore {
		store := storeTag(storeID)
		threshold := f.calcHotThreshold(stats, storeID)
		hotCacheStatusGauge.WithLabelValues("total_length", store, typ).Set(float64(peers.Len()))
		hotCacheStatusGauge.WithLabelValues("hotThreshold", store, typ).Set(float64(threshold))
	}
}

func (f *hotPeerCache) getBytesFlow(region *core.RegionInfo) uint64 {
	switch f.kind {
	case WriteFlow:
		return region.GetBytesWritten()
	case ReadFlow:
		return region.GetBytesRead()
	}
	return 0
}

func (f *hotPeerCache) getKeysFlow(region *core.RegionInfo) uint64 {
	switch f.kind {
	case WriteFlow:
		return region.GetKeysWritten()
	case ReadFlow:
		return region.GetKeysRead()
	}
	return 0
}

func (f *hotPeerCache) getOldHotPeerStat(regionID, storeID uint64) *HotPeerStat {
	if hotPeers, ok := f.peersOfStore[storeID]; ok {
		if v, ok := hotPeers.Peek(regionID); ok {
			return v.(*HotPeerStat)
		}
	}
	return nil
}

func (f *hotPeerCache) isRegionExpired(region *core.RegionInfo, storeID uint64) bool {
	switch f.kind {
	case WriteFlow:
		return region.GetStorePeer(storeID) == nil
	case ReadFlow:
		return region.GetLeader().GetStoreId() != storeID
	}
	return false
}

func (f *hotPeerCache) calcHotThreshold(stats *StoresStats, storeID uint64) uint64 {
	switch f.kind {
	case WriteFlow:
		return calculateWriteHotThresholdWithStore(stats, storeID)
	case ReadFlow:
		return calculateReadHotThresholdWithStore(stats, storeID)
	}
	return 0
}

// gets the storeIDs, including old region and new region
func (f *hotPeerCache) getAllStoreIDs(region *core.RegionInfo) map[uint64]struct{} {
	storeIDs := make(map[uint64]struct{})
	// old stores
	ids, ok := f.storesOfRegion[region.GetID()]
	if ok {
		for storeID := range ids {
			storeIDs[storeID] = struct{}{}
		}
	}

	// new stores
	for _, peer := range region.GetPeers() {
		// ReadFlow no need consider the followers.
		if f.kind == ReadFlow && peer.GetStoreId() != region.GetLeader().GetStoreId() {
			continue
		}
		if _, ok := storeIDs[peer.GetStoreId()]; !ok {
			storeIDs[peer.GetStoreId()] = struct{}{}
		}
	}

	return storeIDs
}

func (f *hotPeerCache) isRegionHotWithAnyPeers(region *core.RegionInfo, hotDegree int) bool {
	for _, peer := range region.GetPeers() {
		if f.isRegionHotWithPeer(region, peer, hotDegree) {
			return true
		}
	}
	return false
}

func (f *hotPeerCache) isRegionHotWithPeer(region *core.RegionInfo, peer *metapb.Peer, hotDegree int) bool {
	if peer == nil {
		return false
	}
	storeID := peer.GetStoreId()
	if peers, ok := f.peersOfStore[storeID]; ok {
		if stat, ok := peers.Peek(region.GetID()); ok {
			return stat.(*HotPeerStat).HotDegree >= hotDegree
		}
	}
	return false
}

func updateHotPeerStat(newItem, oldItem *HotPeerStat, bytesRate uint64, hotThreshold uint64) *HotPeerStat {
	isHot := bytesRate >= hotThreshold
	if newItem.needDelete {
		return newItem
	}
	if oldItem != nil {
		newItem.RollingBytesRate = oldItem.RollingBytesRate
		if isHot {
			newItem.HotDegree = oldItem.HotDegree + 1
			newItem.AntiCount = hotRegionAntiCount
		} else {
			newItem.HotDegree = oldItem.HotDegree - 1
			newItem.AntiCount = oldItem.AntiCount - 1
			if newItem.AntiCount < 0 {
				newItem.needDelete = true
			}
		}
	} else {
		if !isHot {
			return nil
		}
		newItem.RollingBytesRate = NewRollingStats(rollingWindowsSize)
		newItem.AntiCount = hotRegionAntiCount
		newItem.isNew = true
	}
	newItem.RollingBytesRate.Add(float64(bytesRate))

	return newItem
}

// Utils
func calculateWriteHotThresholdWithStore(stats *StoresStats, storeID uint64) uint64 {
	writeBytes, _ := stats.GetStoreBytesRate(storeID)
	divisor := float64(hotPeerMaxCount)
	hotRegionThreshold := uint64(float64(writeBytes) / divisor)

	if hotRegionThreshold < hotWriteRegionMinFlowRate {
		hotRegionThreshold = hotWriteRegionMinFlowRate
	}
	return hotRegionThreshold
}

func calculateReadHotThresholdWithStore(stats *StoresStats, storeID uint64) uint64 {
	_, readBytes := stats.GetStoreBytesRate(storeID)
	divisor := float64(hotPeerMaxCount)
	hotRegionThreshold := uint64(float64(readBytes) / divisor)

	if hotRegionThreshold < hotReadRegionMinFlowRate {
		hotRegionThreshold = hotReadRegionMinFlowRate
	}
	return hotRegionThreshold
}
