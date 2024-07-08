package schemastore

import (
	"sync"

	"github.com/pingcap/log"
	"go.uber.org/zap"

	"github.com/google/btree"
)

type unSortedDDLCache struct {
	mutex sync.Mutex
	// orderd by commitTS
	// TODO: whether need a startTS?
	ddlEvents *btree.BTreeG[DDLEvent]
}

func ddlEventLess(a, b DDLEvent) bool {
	// TODO: do we need finished ts?
	return a.CommitTS < b.CommitTS
}

func newUnSortedDDLCache() *unSortedDDLCache {
	return &unSortedDDLCache{
		ddlEvents: btree.NewG[DDLEvent](16, ddlEventLess),
	}
}

func (c *unSortedDDLCache) addDDL(ddlEvent DDLEvent) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// TODO: is commitTS unique?
	oldEvent, duplicated := c.ddlEvents.ReplaceOrInsert(ddlEvent)
	if duplicated {
		log.Fatal("commitTS conflict", zap.Any("oldEvent", oldEvent), zap.Any("newEvent", ddlEvent))
	}
}

func (c *unSortedDDLCache) getSortedDDLEventBeforeTS(ts Timestamp) []DDLEvent {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	events := make([]DDLEvent, 0)
	c.ddlEvents.Ascend(func(event DDLEvent) bool {
		if event.CommitTS <= ts {
			events = append(events, event)
			return true
		}
		return false
	})
	for _, event := range events {
		c.ddlEvents.Delete(event)
	}
	return events
}