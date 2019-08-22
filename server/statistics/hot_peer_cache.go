package statistics

import (
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/pd/pkg/cache"
	"github.com/pingcap/pd/server/core"
)

const rollingWindowsSize = 5

// hotPeerCache saves the hotspot peer's statistics.
type hotPeerCache struct {
	kind            FlowKind
	storePeersMap   map[uint64]cache.Cache         // storeID -> hot peers
	regionStoresMap map[uint64]map[uint64]struct{} // regionID -> storeIDs
}

// NewHotStoresStats creates a HotStoresStats
func NewHotStoresStats(kind FlowKind) *hotPeerCache {
	return &hotPeerCache{
		kind:            kind,
		storePeersMap:   make(map[uint64]cache.Cache),
		regionStoresMap: make(map[uint64]map[uint64]struct{}),
	}
}

// Update updates the items in statistics.
func (f *hotPeerCache) Update(item *HotPeerStat) {
	if item.IsNeedDelete() {
		if hotStoreStat, ok := f.storePeersMap[item.StoreID]; ok {
			hotStoreStat.Remove(item.RegionID)
		}

		if index, ok := f.regionStoresMap[item.RegionID]; ok {
			delete(index, item.StoreID)
		}
	} else {
		hotStoreStat, ok := f.storePeersMap[item.StoreID]
		if !ok {
			hotStoreStat = cache.NewCache(statCacheMaxLen, cache.TwoQueueCache)
			f.storePeersMap[item.StoreID] = hotStoreStat
		}
		hotStoreStat.Put(item.RegionID, item)

		index, ok := f.regionStoresMap[item.RegionID]
		if !ok {
			index = make(map[uint64]struct{})
			f.regionStoresMap[item.RegionID] = index
		}
		index[item.StoreID] = struct{}{}
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
		var oldItem *HotPeerStat

		hotStoreStats, ok := f.storePeersMap[storeID]
		if ok {
			if v, isExist := hotStoreStats.Peek(region.GetID()); isExist {
				oldItem = v.(*HotPeerStat)
				// This is used for the simulator.
				if Denoising {
					interval := time.Since(oldItem.LastUpdateTime).Seconds()
					// ignore if report too fast
					if interval < minHotRegionReportInterval && !f.isRegionExpired(region, storeID) {
						continue
					}
					bytesPerSec = uint64(float64(bytesFlow) / interval)
					keysPerSec = uint64(float64(keysFlow) / interval)
				}
			}
		}

		isExpired := f.isRegionExpired(region, storeID)
		isLeader := region.GetLeader().GetStoreId() == storeID
		isHot := bytesPerSec >= f.calcHotThreshold(storeID, stats)

		newItem := &HotPeerStat{
			StoreID:        storeID,
			RegionID:       region.GetID(),
			Kind:           f.kind,
			BytesRate:      bytesPerSec,
			KeysRate:       keysPerSec,
			LastUpdateTime: time.Now(),
			Version:        region.GetMeta().GetRegionEpoch().GetVersion(),
			needDelete:     isExpired,
			isLeader:       isLeader,
		}

		newItem = updateHotPeerStat(newItem, oldItem, bytesPerSec, keysPerSec, isHot)
		if newItem != nil {
			ret = append(ret, newItem)
		}
	}

	return ret
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

func (f *hotPeerCache) isRegionExpired(region *core.RegionInfo, storeID uint64) bool {
	switch f.kind {
	case WriteFlow:
		return region.GetStorePeer(storeID) == nil
	case ReadFlow:
		return region.GetLeader().GetStoreId() != storeID
	}
	return false
}

func (f *hotPeerCache) calcHotThreshold(storeID uint64, stats *StoresStats) uint64 {
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
	ids, ok := f.regionStoresMap[region.GetID()]
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

func (f *hotPeerCache) isRegionHotWithAnyPeers(region *core.RegionInfo, hotThreshold int) bool {
	for _, peer := range region.GetPeers() {
		if f.isRegionHotWithPeer(region, peer, hotThreshold) {
			return true
		}
	}
	return false

}

func (f *hotPeerCache) isRegionHotWithPeer(region *core.RegionInfo, peer *metapb.Peer, hotThreshold int) bool {
	if peer == nil {
		return false
	}
	storeID := peer.GetStoreId()
	stats, ok := f.storePeersMap[storeID]
	if !ok {
		return false
	}
	if stat, ok := stats.Peek(region.GetID()); ok {
		return stat.(*HotPeerStat).HotDegree >= hotThreshold
	}
	return false
}

func updateHotPeerStat(newItem, oldItem *HotPeerStat, bytesRate, keysRate uint64, isHot bool) *HotPeerStat {
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
