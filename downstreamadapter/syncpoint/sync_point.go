package syncpoint

import (
	"time"

	"github.com/tikv/client-go/v2/oracle"
)

// SyncPointConfig not nil only when enable sync point
type SyncPointConfig struct {
	SyncPointInterval  time.Duration
	SyncPointRetention time.Duration
}

func CalculateStartSyncPointTs(startTs uint64, syncPointInterval time.Duration) uint64 {
	if syncPointInterval == time.Duration(0) {
		return 0
	}
	k := oracle.GetTimeFromTS(startTs).Sub(time.Unix(0, 0)) / syncPointInterval
	if oracle.GetTimeFromTS(startTs).Sub(time.Unix(0, 0))%syncPointInterval != 0 || oracle.ExtractLogical(startTs) != 0 {
		k += 1
	}
	return oracle.GoTimeToTS(time.Unix(0, 0).Add(k * syncPointInterval))
}
