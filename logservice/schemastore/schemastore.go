package schemastore

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/flowbehappy/tigate/logservice/logpuller"
	"github.com/flowbehappy/tigate/pkg/common"
	"github.com/pingcap/log"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tiflow/pkg/pdutil"
	"github.com/tikv/client-go/v2/tikv"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type SchemaStore interface {
	Run(ctx context.Context) error

	Close(ctx context.Context)

	// TODO: add filter
	GetAllPhysicalTables(snapTs common.Ts) ([]common.TableID, error)

	// RegisterDispatcher register the dispatcher into the schema store.
	// TODO: return a table info
	// TODO: add filter
	RegisterDispatcher(dispatcherID common.DispatcherID, tableID common.TableID, ts common.Ts) error

	// TODO: add interface for TableEventDispatcher

	UpdateDispatcherSendTS(dispatcherID common.DispatcherID, ts common.Ts) error

	UnregisterDispatcher(dispatcherID common.DispatcherID) error

	GetMaxFinishedDDLTS() common.Ts

	GetTableInfo(tableID common.TableID, ts common.Ts) (*common.TableInfo, error)

	GetNextDDLEvent(dispatcherID common.DispatcherID) (*DDLEvent, common.Ts, error)
}

type schemaStore struct {
	ddlJobFetcher *ddlJobFetcher

	storage kv.Storage

	// store unresolved ddl event in memory, it is thread safe
	unsortedCache *unsortedDDLCache

	// store ddl event and other metadata on disk, it is thread safe
	dataStorage *persistentStorage

	eventCh chan interface{}

	// all following fields are guarded by this mutex
	mu sync.Mutex

	// max finishedTS of all applied ddl events
	finishedDDLTS common.Ts
	// max schemaVersion of all applied ddl events
	schemaVersion int64

	// databaseID -> database info
	// it contains all databases
	databaseMap DatabaseInfoMap

	// tableID -> versioned store
	// it just contains tables which have registered dispatchers
	tableInfoStoreMap TableInfoStoreMap

	// dispatcherID -> dispatch info
	// TODO: how to deal with table event dispatchers？
	dispatchersMap DispatcherInfoMap
}

func NewSchemaStore(
	root string,
	pdCli pd.Client,
	regionCache *tikv.RegionCache,
	pdClock pdutil.Clock,
	kvStorage kv.Storage,
) (SchemaStore, error) {

	minRequiredTS := common.Ts(0) // FIXME
	dataStorage, metaTS, databaseMap := newPersistentStorage(root, kvStorage, minRequiredTS)

	s := &schemaStore{
		storage:           kvStorage,
		unsortedCache:     newUnSortedDDLCache(),
		dataStorage:       dataStorage,
		eventCh:           make(chan interface{}, 1024),
		finishedDDLTS:     metaTS.finishedDDLTS,
		schemaVersion:     int64(metaTS.schemaVersion),
		databaseMap:       databaseMap,
		tableInfoStoreMap: make(TableInfoStoreMap),
		dispatchersMap:    make(DispatcherInfoMap),
	}
	s.ddlJobFetcher = newDDLJobFetcher(
		pdCli,
		regionCache,
		pdClock,
		kvStorage,
		metaTS.resolvedTS,
		s.writeDDLEvent,
		s.advanceResolvedTs)

	return s, nil
}

func (s *schemaStore) Run(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)
	eg.Go(func() error {
		return s.batchCommitAndUpdateWatermark(ctx)
	})
	eg.Go(func() error {
		return s.ddlJobFetcher.run(ctx)
	})
	return eg.Wait()
}

func (s *schemaStore) Close(ctx context.Context) {

}

// TODO: use a meaningful name
func (s *schemaStore) batchCommitAndUpdateWatermark(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case data := <-s.eventCh:
			switch v := data.(type) {
			case DDLEvent:
				s.unsortedCache.addDDLEvent(v)
				// TODO: batch ddl event
				err := s.dataStorage.writeDDLEvent(v)
				if err != nil {
					log.Fatal("write ddl event failed", zap.Error(err))
				}
			case common.Ts:
				// TODO: check resolved ts is monotonically increasing
				resolvedEvents := s.unsortedCache.fetchSortedDDLEventBeforeTS(v)
				if len(resolvedEvents) == 0 {
					continue
				}
				// TODO: whether the events is ordered by finishedDDLTS and schemaVersion
				newFinishedDDLTS := resolvedEvents[len(resolvedEvents)-1].Job.BinlogInfo.FinishedTS
				newSchemaVersion := resolvedEvents[len(resolvedEvents)-1].Job.Version
				err := s.dataStorage.updateStoreMeta(v, common.Ts(newFinishedDDLTS), common.Ts(newSchemaVersion))
				if err != nil {
					log.Fatal("update ts failed", zap.Error(err))
				}
				s.mu.Lock()
				defer s.mu.Unlock()
				for _, event := range resolvedEvents {
					if event.Job.Version <= s.schemaVersion || event.Job.BinlogInfo.FinishedTS <= uint64(s.finishedDDLTS) {
						log.Warn("skip already applied ddl job",
							zap.Any("job", event.Job),
							zap.Any("schemaVersion", s.schemaVersion),
							zap.Any("finishedDDLTS", s.finishedDDLTS))
						continue
					}
					if err := handleResolvedDDLJob(event.Job, s.databaseMap, s.tableInfoStoreMap); err != nil {
						return err
					}
					s.schemaVersion = event.Job.Version
					s.finishedDDLTS = common.Ts(event.Job.BinlogInfo.FinishedTS)
				}
			default:
				log.Fatal("unknown event type")
			}
		}
	}
}

func isSystemDB(dbName string) bool {
	return dbName == "mysql" || dbName == "sys"
}

func (s *schemaStore) GetAllPhysicalTables(snapTs common.Ts) ([]common.TableID, error) {
	meta := logpuller.GetSnapshotMeta(s.storage, uint64(snapTs))
	start := time.Now()
	dbinfos, err := meta.ListDatabases()
	if err != nil {
		log.Fatal("list databases failed", zap.Error(err))
	}

	tableIDs := make([]common.TableID, 0)

	for _, dbinfo := range dbinfos {
		if isSystemDB(dbinfo.Name.O) {
			continue
		}
		log.Info("get database", zap.Any("dbinfo", dbinfo))
		rawTables, err := meta.GetMetasByDBID(dbinfo.ID)
		if err != nil {
			log.Fatal("get tables failed", zap.Error(err))
		}
		for _, rawTable := range rawTables {
			if !isTableRawKey(rawTable.Field) {
				continue
			}
			tbName := &model.TableNameInfo{}
			err := json.Unmarshal(rawTable.Value, tbName)
			if err != nil {
				log.Fatal("get table info failed", zap.Error(err))
			}
			tableIDs = append(tableIDs, common.TableID(tbName.ID))
		}
	}

	log.Info("finish write schema snapshot",
		zap.Any("duration", time.Since(start).Seconds()))
	return tableIDs, nil
}

func (s *schemaStore) RegisterDispatcher(
	dispatcherID common.DispatcherID, tableID common.TableID, startTS common.Ts,
) error {
	s.mu.Lock()
	if startTS < s.dataStorage.getGCTS() {
		return errors.New("start ts is old than gc ts")
	}
	s.dispatchersMap[dispatcherID] = DispatcherInfo{
		tableID: tableID,
		// filter:  filter,
	}
	getSchemaName := func(schemaID common.SchemaID) (string, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		databaseInfo, ok := s.databaseMap[common.DatabaseID(schemaID)]
		if !ok {
			return "", errors.New("database not found")
		}
		return databaseInfo.Name, nil
	}
	// check whether there is already a versionedTableInfoStore satisfy the needs
	store, ok := s.tableInfoStoreMap[tableID]
	if !ok {
		store = newEmptyVersionedTableInfoStore(tableID)
		s.tableInfoStoreMap[tableID] = store
		store.registerDispatcher(dispatcherID, startTS)
		endTS := s.finishedDDLTS
		s.mu.Unlock()
		err := s.dataStorage.buildVersionedTableInfoStore(store, startTS, endTS, getSchemaName)
		if err != nil {
			// TODO: unregister dispatcher, make sure other wait go routines exit successfully
			return err
		}
		store.setTableInfoInitialized()
		return nil
	} else {
		// prevent old store from gc
		store.registerDispatcher(dispatcherID, startTS)
		s.mu.Unlock()
		store.waitTableInfoInitialized()
	}
	if store.getFirstVersion() <= startTS {
		return nil
	}
	endTS := store.getFirstVersion()

	// TODO: there may be multiple dispatchers build the same versionedTableInfoStore, optimize it later
	newStore := newEmptyVersionedTableInfoStore(tableID)
	err := s.dataStorage.buildVersionedTableInfoStore(newStore, startTS, endTS, getSchemaName)
	if err != nil {
		return err
	}
	newStore.setTableInfoInitialized()

	s.mu.Lock()
	defer s.mu.Unlock()
	// check whether the data is gced again
	if startTS < s.dataStorage.getGCTS() {
		// TODO: unregister dispatcher, make sure other wait go routines exit successfully
		return errors.New("start ts is old than gc ts")
	}
	oldStore, ok := s.tableInfoStoreMap[tableID]
	if ok {
		// Note: oldStore must be initialized, no need to check again.
		// keep the store with smaller version
		if oldStore.getFirstVersion() <= newStore.getFirstVersion() {
			return nil
		} else {
			newStore.checkAndCopyTailFrom(oldStore)
			newStore.copyRegisteredDispatchers(oldStore)
			s.tableInfoStoreMap[tableID] = newStore
		}
	} else {
		log.Panic("should not happened")
	}

	return nil
}

func (s *schemaStore) UpdateDispatcherSendTS(dispatcherID common.DispatcherID, ts common.Ts) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.dispatchersMap[dispatcherID]
	if !ok {
		return errors.New("dispatcher not found")
	}
	store := s.tableInfoStoreMap[common.TableID(info.tableID)]
	store.updateDispatcherSendTS(dispatcherID, ts)
	return nil
}

func (s *schemaStore) UnregisterDispatcher(dispatcherID common.DispatcherID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.dispatchersMap[dispatcherID]
	if !ok {
		return errors.New("dispatcher not found")
	}
	tableID := info.tableID
	delete(s.dispatchersMap, dispatcherID)
	store := s.tableInfoStoreMap[tableID]
	removed := store.unregisterDispatcher(dispatcherID)
	if removed {
		delete(s.tableInfoStoreMap, tableID)
	}
	return nil
}

func (s *schemaStore) GetMaxFinishedDDLTS() common.Ts {
	s.mu.Lock()
	defer s.mu.Lock()
	return s.finishedDDLTS
}

func (s *schemaStore) GetTableInfo(tableID common.TableID, ts common.Ts) (*common.TableInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	store, ok := s.tableInfoStoreMap[tableID]
	if !ok {
		return nil, errors.New("table not found")
	}
	store.waitTableInfoInitialized()
	return store.getTableInfo(ts)
}

func (s *schemaStore) GetNextDDLEvent(dispatcherID common.DispatcherID) (*DDLEvent, common.Ts, error) {
	return nil, 0, nil
}

func (s *schemaStore) writeDDLEvent(ddlEvent DDLEvent) error {
	// log.Info("write ddl event", zap.Any("ddlEvent", ddlEvent))
	s.eventCh <- ddlEvent
	return nil
}

func (s *schemaStore) advanceResolvedTs(resolvedTs common.Ts) error {
	// log.Info("advance resolved ts", zap.Any("resolvedTS", resolvedTs))
	s.eventCh <- resolvedTs
	return nil
}

// TODO: run gc when calling schemaStore.run
func (s *schemaStore) doGC() error {
	// fetch gcTs from upstream
	gcTs := common.Ts(0)
	// TODO: gc databaseMap
	return s.dataStorage.gc(gcTs)
}

func handleResolvedDDLJob(job *model.Job, databaseMap DatabaseInfoMap, tableInfoStoreMap TableInfoStoreMap) error {
	if err := fillSchemaName(job, databaseMap); err != nil {
		return err
	}

	switch job.Type {
	case model.ActionCreateSchema:
		return createSchema(job, databaseMap)
	case model.ActionModifySchemaCharsetAndCollate:
		// ignore
		return nil
	case model.ActionDropSchema:
		return dropSchema(job, databaseMap)
	case model.ActionRenameTables:
		var oldSchemaIDs, newSchemaIDs, oldTableIDs []int64
		var newTableNames, oldSchemaNames []*model.CIStr
		err := job.DecodeArgs(&oldSchemaIDs, &newSchemaIDs, &newTableNames, &oldTableIDs, &oldSchemaNames)
		if err != nil {
			return err
		}
	case model.ActionCreateTables,
		model.ActionCreateTable,
		model.ActionCreateView,
		model.ActionRecoverTable:
		// no dispatcher should register on these kinds of tables?
		// TODO: add a cache for these kinds of newly created tables because they may soon be registered?
		if _, ok := tableInfoStoreMap[common.TableID(job.TableID)]; ok {
			log.Panic("should not happened")
		}
		return nil
	default:
		tableID := common.TableID(job.TableID)
		store, ok := tableInfoStoreMap[tableID]
		if !ok {
			return errors.New("table not found")
		}
		store.applyDDL(job)
	}

	return nil
}

func fillSchemaName(job *model.Job, databaseMap DatabaseInfoMap) error {
	databaseID := common.DatabaseID(job.SchemaID)
	databaseInfo, ok := databaseMap[databaseID]
	if !ok {
		return errors.New("database not found")
	}
	if databaseInfo.CreateVersion > common.Ts(job.BinlogInfo.FinishedTS) {
		return errors.New("database is not created")
	}
	if databaseInfo.DeleteVersion < common.Ts(job.BinlogInfo.FinishedTS) {
		return errors.New("database is deleted")
	}
	job.SchemaName = databaseInfo.Name
	return nil
}

func createSchema(job *model.Job, databaseMap DatabaseInfoMap) error {
	if _, ok := databaseMap[common.DatabaseID(job.SchemaID)]; ok {
		return errors.New("database already exists")
	}
	databaseInfo := &DatabaseInfo{
		Name:          job.SchemaName,
		Tables:        make([]common.TableID, 0),
		CreateVersion: common.Ts(job.BinlogInfo.FinishedTS),
		DeleteVersion: math.MaxUint64,
	}
	databaseMap[common.DatabaseID(job.SchemaID)] = databaseInfo
	return nil
}

func dropSchema(job *model.Job, databaseMap DatabaseInfoMap) error {
	databaseInfo, ok := databaseMap[common.DatabaseID(job.SchemaID)]
	if !ok {
		return errors.New("database not found")
	}
	if databaseInfo.isDeleted() {
		return errors.New("database is already deleted")
	}
	databaseInfo.DeleteVersion = common.Ts(job.BinlogInfo.FinishedTS)
	return nil
}
