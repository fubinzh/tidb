// Copyright 2024 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package infoschema

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/pkg/ddl/placement"
	"github.com/pingcap/tidb/pkg/kv"
	"github.com/pingcap/tidb/pkg/meta"
	"github.com/pingcap/tidb/pkg/meta/autoid"
	"github.com/pingcap/tidb/pkg/metrics"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/table"
	"github.com/pingcap/tidb/pkg/table/tables"
	"github.com/pingcap/tidb/pkg/util"
	"github.com/pingcap/tidb/pkg/util/size"
	"github.com/pingcap/tidb/pkg/util/tracing"
	"github.com/tidwall/btree"
	"golang.org/x/sync/singleflight"
)

// tableItem is the btree item sorted by name or by id.
type tableItem struct {
	dbName        string
	dbID          int64
	tableName     string
	tableID       int64
	schemaVersion int64
	tomb          bool
}

type schemaItem struct {
	schemaVersion int64
	dbInfo        *model.DBInfo
	tomb          bool
}

type schemaIDName struct {
	schemaVersion int64
	id            int64
	name          string
	tomb          bool
}

func (si *schemaItem) Name() string {
	return si.dbInfo.Name.L
}

// versionAndTimestamp is the tuple of schema version and timestamp.
type versionAndTimestamp struct {
	schemaVersion int64
	timestamp     uint64
}

// Data is the core data struct of infoschema V2.
type Data struct {
	// For the TableByName API, sorted by {dbName, tableName, schemaVersion} => tableID
	//
	// If the schema version +1 but a specific table does not change, the old record is
	// kept and no new {dbName, tableName, schemaVersion+1} => tableID record been added.
	//
	// It means as long as we can find an item in it, the item is available, even through the
	// schema version maybe smaller than required.
	byName *btree.BTreeG[tableItem]

	// For the TableByID API, sorted by {tableID, schemaVersion} => dbID
	// To reload model.TableInfo, we need both table ID and database ID for meta kv API.
	// It provides the tableID => databaseID mapping.
	// This mapping MUST be synced with byName.
	byID *btree.BTreeG[tableItem]

	// For the SchemaByName API, sorted by {dbName, schemaVersion} => model.DBInfo
	// Stores the full data in memory.
	schemaMap *btree.BTreeG[schemaItem]

	// For the SchemaByID API, sorted by {id, schemaVersion}
	// Stores only id, name and schemaVersion in memory.
	schemaID2Name *btree.BTreeG[schemaIDName]

	tableCache *Sieve[tableCacheKey, table.Table]

	// sorted by both SchemaVersion and timestamp in descending order, assume they have same order
	mu struct {
		sync.RWMutex
		versionTimestamps []versionAndTimestamp
	}

	// For information_schema/metrics_schema/performance_schema etc
	specials sync.Map

	// pid2tid is used by FindTableInfoByPartitionID, it stores {partitionID, schemaVersion} => table ID
	// Need full data in memory!
	pid2tid *btree.BTreeG[partitionItem]

	// tableInfoResident stores {dbName, tableID, schemaVersion} => model.TableInfo
	// It is part of the model.TableInfo data kept in memory to accelerate the list tables API.
	// We observe the pattern that list table API always come with filter.
	// All model.TableInfo with special attributes are here, currently the special attributes including:
	//     TTLInfo, TiFlashReplica
	// PlacementPolicyRef, Partition might be added later, and also ForeignKeys, TableLock etc
	tableInfoResident *btree.BTreeG[tableInfoItem]
}

type tableInfoItem struct {
	dbName        string
	tableID       int64
	schemaVersion int64
	tableInfo     *model.TableInfo
	tomb          bool
}

type partitionItem struct {
	partitionID   int64
	schemaVersion int64
	tableID       int64
	tomb          bool
}

func (isd *Data) getVersionByTS(ts uint64) (int64, bool) {
	isd.mu.RLock()
	defer isd.mu.RUnlock()
	return isd.getVersionByTSNoLock(ts)
}

func (isd *Data) getVersionByTSNoLock(ts uint64) (int64, bool) {
	// search one by one instead of binary search, because the timestamp of a schema could be 0
	// this is ok because the size of h.tableCache is small (currently set to 16)
	// moreover, the most likely hit element in the array is the first one in steady mode
	// thus it may have better performance than binary search
	for i, vt := range isd.mu.versionTimestamps {
		if vt.timestamp == 0 || ts < vt.timestamp {
			// is.timestamp == 0 means the schema ts is unknown, so we can't use it, then just skip it.
			// ts < is.timestamp means the schema is newer than ts, so we can't use it too, just skip it to find the older one.
			continue
		}
		// ts >= is.timestamp must be true after the above condition.
		if i == 0 {
			// the first element is the latest schema, so we can return it directly.
			return vt.schemaVersion, true
		}
		if isd.mu.versionTimestamps[i-1].schemaVersion == vt.schemaVersion+1 && isd.mu.versionTimestamps[i-1].timestamp > ts {
			// This first condition is to make sure the schema version is continuous. If last(cache[i-1]) schema-version is 10,
			// but current(cache[i]) schema-version is not 9, then current schema is not suitable for ts.
			// The second condition is to make sure the cache[i-1].timestamp > ts >= cache[i].timestamp, then the current schema is suitable for ts.
			return vt.schemaVersion, true
		}
		// current schema is not suitable for ts, then break the loop to avoid the unnecessary search.
		break
	}

	return 0, false
}

type tableCacheKey struct {
	tableID       int64
	schemaVersion int64
}

// NewData creates an infoschema V2 data struct.
func NewData() *Data {
	ret := &Data{
		byID:              btree.NewBTreeG[tableItem](compareByID),
		byName:            btree.NewBTreeG[tableItem](compareByName),
		schemaMap:         btree.NewBTreeG[schemaItem](compareSchemaItem),
		schemaID2Name:     btree.NewBTreeG[schemaIDName](compareSchemaByID),
		tableCache:        newSieve[tableCacheKey, table.Table](1024 * 1024 * size.MB),
		pid2tid:           btree.NewBTreeG[partitionItem](comparePartitionItem),
		tableInfoResident: btree.NewBTreeG[tableInfoItem](compareTableInfoItem),
	}
	ret.tableCache.SetStatusHook(newSieveStatusHookImpl())
	return ret
}

// CacheCapacity is exported for testing.
func (isd *Data) CacheCapacity() uint64 {
	return isd.tableCache.Capacity()
}

// SetCacheCapacity sets the cache capacity size in bytes.
func (isd *Data) SetCacheCapacity(capacity uint64) {
	isd.tableCache.SetCapacityAndWaitEvict(capacity)
}

func (isd *Data) add(item tableItem, tbl table.Table) {
	isd.byID.Set(item)
	isd.byName.Set(item)
	isd.tableCache.Set(tableCacheKey{item.tableID, item.schemaVersion}, tbl)
	ti := tbl.Meta()
	if pi := ti.GetPartitionInfo(); pi != nil {
		for _, def := range pi.Definitions {
			isd.pid2tid.Set(partitionItem{def.ID, item.schemaVersion, tbl.Meta().ID, false})
		}
	}
	if hasSpecialAttributes(ti) {
		isd.tableInfoResident.Set(tableInfoItem{
			dbName:        item.dbName,
			tableID:       item.tableID,
			schemaVersion: item.schemaVersion,
			tableInfo:     ti,
			tomb:          false})
	}
}

func (isd *Data) addSpecialDB(di *model.DBInfo, tables *schemaTables) {
	isd.specials.LoadOrStore(di.Name.L, tables)
}

func (isd *Data) addDB(schemaVersion int64, dbInfo *model.DBInfo) {
	dbInfo.Tables = nil
	isd.schemaID2Name.Set(schemaIDName{schemaVersion: schemaVersion, id: dbInfo.ID, name: dbInfo.Name.O})
	isd.schemaMap.Set(schemaItem{schemaVersion: schemaVersion, dbInfo: dbInfo})
}

func (isd *Data) remove(item tableItem) {
	item.tomb = true
	isd.byID.Set(item)
	isd.byName.Set(item)
	isd.tableInfoResident.Set(tableInfoItem{
		dbName:        item.dbName,
		tableID:       item.tableID,
		schemaVersion: item.schemaVersion,
		tableInfo:     nil,
		tomb:          true})
	isd.tableCache.Remove(tableCacheKey{item.tableID, item.schemaVersion})
}

func (isd *Data) deleteDB(dbInfo *model.DBInfo, schemaVersion int64) {
	item := schemaItem{schemaVersion: schemaVersion, dbInfo: dbInfo, tomb: true}
	isd.schemaMap.Set(item)
	isd.schemaID2Name.Set(schemaIDName{schemaVersion: schemaVersion, id: dbInfo.ID, name: dbInfo.Name.O, tomb: true})
}

func compareByID(a, b tableItem) bool {
	if a.tableID < b.tableID {
		return true
	}
	if a.tableID > b.tableID {
		return false
	}

	return a.schemaVersion < b.schemaVersion
}

func compareByName(a, b tableItem) bool {
	if a.dbName < b.dbName {
		return true
	}
	if a.dbName > b.dbName {
		return false
	}

	if a.tableName < b.tableName {
		return true
	}
	if a.tableName > b.tableName {
		return false
	}

	return a.schemaVersion < b.schemaVersion
}

func compareTableInfoItem(a, b tableInfoItem) bool {
	if a.dbName < b.dbName {
		return true
	}
	if a.dbName > b.dbName {
		return false
	}

	if a.tableID < b.tableID {
		return true
	}
	if a.tableID > b.tableID {
		return false
	}
	return a.schemaVersion < b.schemaVersion
}

func comparePartitionItem(a, b partitionItem) bool {
	if a.partitionID < b.partitionID {
		return true
	}
	if a.partitionID > b.partitionID {
		return false
	}
	return a.schemaVersion < b.schemaVersion
}

func compareSchemaItem(a, b schemaItem) bool {
	if a.Name() < b.Name() {
		return true
	}
	if a.Name() > b.Name() {
		return false
	}
	return a.schemaVersion < b.schemaVersion
}

func compareSchemaByID(a, b schemaIDName) bool {
	if a.id < b.id {
		return true
	}
	if a.id > b.id {
		return false
	}
	return a.schemaVersion < b.schemaVersion
}

var _ InfoSchema = &infoschemaV2{}

type infoschemaV2 struct {
	*infoSchema // in fact, we only need the infoSchemaMisc inside it, but the builder rely on it.
	r           autoid.Requirement
	ts          uint64
	*Data
}

// NewInfoSchemaV2 create infoschemaV2.
func NewInfoSchemaV2(r autoid.Requirement, infoData *Data) infoschemaV2 {
	return infoschemaV2{
		infoSchema: newInfoSchema(),
		Data:       infoData,
		r:          r,
	}
}

func search(bt *btree.BTreeG[tableItem], schemaVersion int64, end tableItem, matchFn func(a, b *tableItem) bool) (tableItem, bool) {
	var ok bool
	var target tableItem
	// Iterate through the btree, find the query item whose schema version is the largest one (latest).
	bt.Descend(end, func(item tableItem) bool {
		if !matchFn(&end, &item) {
			return false
		}
		if item.schemaVersion > schemaVersion {
			// We're seaching historical snapshot, and this record is newer than us, we can't use it.
			// Skip the record.
			return true
		}
		// schema version of the items should <= query's schema version.
		if !ok { // The first one found.
			ok = true
			target = item
		} else { // The latest one
			if item.schemaVersion > target.schemaVersion {
				target = item
			}
		}
		return true
	})
	if ok && target.tomb {
		// If the item is a tomb record, the table is dropped.
		ok = false
	}
	return target, ok
}

func (is *infoschemaV2) base() *infoSchema {
	return is.infoSchema
}

func (is *infoschemaV2) CloneAndUpdateTS(startTS uint64) *infoschemaV2 {
	tmp := *is
	tmp.ts = startTS
	return &tmp
}

func (is *infoschemaV2) TableByID(id int64) (val table.Table, ok bool) {
	return is.tableByID(id, false)
}

func (is *infoschemaV2) tableByID(id int64, noRefill bool) (val table.Table, ok bool) {
	if !tableIDIsValid(id) {
		return
	}

	// Get from the cache.
	key := tableCacheKey{id, is.infoSchema.schemaMetaVersion}
	tbl, found := is.tableCache.Get(key)
	if found && tbl != nil {
		return tbl, true
	}

	eq := func(a, b *tableItem) bool { return a.tableID == b.tableID }
	itm, ok := search(is.byID, is.infoSchema.schemaMetaVersion, tableItem{tableID: id, schemaVersion: math.MaxInt64}, eq)
	if !ok {
		return nil, false
	}

	if isTableVirtual(id) {
		if raw, exist := is.Data.specials.Load(itm.dbName); exist {
			schTbls := raw.(*schemaTables)
			val, ok = schTbls.tables[itm.tableName]
			return
		}
		return nil, false
	}
	// get cache with old key
	oldKey := tableCacheKey{itm.tableID, itm.schemaVersion}
	tbl, found = is.tableCache.Get(oldKey)
	if found && tbl != nil {
		if !noRefill {
			is.tableCache.Set(key, tbl)
		}
		return tbl, true
	}

	// Maybe the table is evicted? need to reload.
	ret, err := loadTableInfo(context.Background(), is.r, is.Data, id, itm.dbID, is.ts, is.infoSchema.schemaMetaVersion)
	if err != nil || ret == nil {
		return nil, false
	}

	if !noRefill {
		is.tableCache.Set(oldKey, ret)
	}
	return ret, true
}

// IsSpecialDB tells whether the database is a special database.
func IsSpecialDB(dbName string) bool {
	return dbName == util.InformationSchemaName.L ||
		dbName == util.PerformanceSchemaName.L ||
		dbName == util.MetricSchemaName.L
}

// EvictTable is exported for testing only.
func (is *infoschemaV2) EvictTable(schema, tbl string) {
	eq := func(a, b *tableItem) bool { return a.dbName == b.dbName && a.tableName == b.tableName }
	itm, ok := search(is.byName, is.infoSchema.schemaMetaVersion, tableItem{dbName: schema, tableName: tbl, schemaVersion: math.MaxInt64}, eq)
	if !ok {
		return
	}
	is.tableCache.Remove(tableCacheKey{itm.tableID, is.infoSchema.schemaMetaVersion})
	is.tableCache.Remove(tableCacheKey{itm.tableID, itm.schemaVersion})
}

func (is *infoschemaV2) TableByName(ctx context.Context, schema, tbl model.CIStr) (t table.Table, err error) {
	if IsSpecialDB(schema.L) {
		if raw, ok := is.specials.Load(schema.L); ok {
			tbNames := raw.(*schemaTables)
			if t, ok = tbNames.tables[tbl.L]; ok {
				return
			}
		}
		return nil, ErrTableNotExists.FastGenByArgs(schema, tbl)
	}

	start := time.Now()
	eq := func(a, b *tableItem) bool { return a.dbName == b.dbName && a.tableName == b.tableName }
	itm, ok := search(is.byName, is.infoSchema.schemaMetaVersion, tableItem{dbName: schema.L, tableName: tbl.L, schemaVersion: math.MaxInt64}, eq)
	if !ok {
		return nil, ErrTableNotExists.FastGenByArgs(schema, tbl)
	}

	// Get from the cache with old key
	oldKey := tableCacheKey{itm.tableID, itm.schemaVersion}
	res, found := is.tableCache.Get(oldKey)
	if found && res != nil {
		metrics.TableByNameHitDuration.Observe(float64(time.Since(start)))
		return res, nil
	}

	// Maybe the table is evicted? need to reload.
	ret, err := loadTableInfo(ctx, is.r, is.Data, itm.tableID, itm.dbID, is.ts, is.infoSchema.schemaMetaVersion)
	if err != nil {
		return nil, errors.Trace(err)
	}
	is.tableCache.Set(oldKey, ret)
	metrics.TableByNameMissDuration.Observe(float64(time.Since(start)))
	return ret, nil
}

// TableInfoByName implements InfoSchema.TableInfoByName
func (is *infoschemaV2) TableInfoByName(schema, table model.CIStr) (*model.TableInfo, error) {
	tbl, err := is.TableByName(context.Background(), schema, table)
	return getTableInfo(tbl), err
}

// TableInfoByID implements InfoSchema.TableInfoByID
func (is *infoschemaV2) TableInfoByID(id int64) (*model.TableInfo, bool) {
	tbl, ok := is.TableByID(id)
	return getTableInfo(tbl), ok
}

// SchemaTableInfos implements InfoSchema.FindTableInfoByPartitionID
func (is *infoschemaV2) SchemaTableInfos(schema model.CIStr) []*model.TableInfo {
	if IsSpecialDB(schema.L) {
		raw, ok := is.Data.specials.Load(schema.L)
		if ok {
			schTbls := raw.(*schemaTables)
			tables := make([]table.Table, 0, len(schTbls.tables))
			for _, tbl := range schTbls.tables {
				tables = append(tables, tbl)
			}
			return getTableInfoList(tables)
		}
		return nil // something wrong?
	}

retry:
	dbInfo, ok := is.SchemaByName(schema)
	if !ok {
		return nil
	}
	snapshot := is.r.Store().GetSnapshot(kv.NewVersion(is.ts))
	// Using the KV timeout read feature to address the issue of potential DDL lease expiration when
	// the meta region leader is slow.
	snapshot.SetOption(kv.TiKVClientReadTimeout, uint64(3000)) // 3000ms.
	m := meta.NewSnapshotMeta(snapshot)
	tblInfos, err := m.ListTables(dbInfo.ID)
	if err != nil {
		if meta.ErrDBNotExists.Equal(err) {
			return nil
		}
		// Flashback statement could cause such kind of error.
		// In theory that error should be handled in the lower layer, like client-go.
		// But it's not done, so we retry here.
		if strings.Contains(err.Error(), "in flashback progress") {
			time.Sleep(200 * time.Millisecond)
			goto retry
		}
		// TODO: error could happen, so do not panic!
		panic(err)
	}
	return tblInfos
}

// FindTableInfoByPartitionID implements InfoSchema.FindTableInfoByPartitionID
func (is *infoschemaV2) FindTableInfoByPartitionID(
	partitionID int64,
) (*model.TableInfo, *model.DBInfo, *model.PartitionDefinition) {
	tbl, db, partDef := is.FindTableByPartitionID(partitionID)
	return getTableInfo(tbl), db, partDef
}

func (is *infoschemaV2) SchemaByName(schema model.CIStr) (val *model.DBInfo, ok bool) {
	if IsSpecialDB(schema.L) {
		raw, ok := is.Data.specials.Load(schema.L)
		if !ok {
			return nil, false
		}
		schTbls, ok := raw.(*schemaTables)
		return schTbls.dbInfo, ok
	}

	var dbInfo model.DBInfo
	dbInfo.Name = schema
	is.Data.schemaMap.Descend(schemaItem{
		dbInfo:        &dbInfo,
		schemaVersion: math.MaxInt64,
	}, func(item schemaItem) bool {
		if item.Name() != schema.L {
			ok = false
			return false
		}
		if item.schemaVersion <= is.infoSchema.schemaMetaVersion {
			if !item.tomb { // If the item is a tomb record, the database is dropped.
				ok = true
				val = item.dbInfo
			}
			return false
		}
		return true
	})
	return
}

func (is *infoschemaV2) allSchemas(visit func(*model.DBInfo)) {
	var last *model.DBInfo
	is.Data.schemaMap.Reverse(func(item schemaItem) bool {
		if item.schemaVersion > is.infoSchema.schemaMetaVersion {
			// Skip the versions that we are not looking for.
			return true
		}

		// Dedup the same db record of different versions.
		if last != nil && last.Name == item.dbInfo.Name {
			return true
		}
		last = item.dbInfo

		if !item.tomb {
			visit(item.dbInfo)
		}
		return true
	})
	is.Data.specials.Range(func(key, value any) bool {
		sc := value.(*schemaTables)
		visit(sc.dbInfo)
		return true
	})
}

func (is *infoschemaV2) AllSchemas() (schemas []*model.DBInfo) {
	is.allSchemas(func(di *model.DBInfo) {
		schemas = append(schemas, di)
	})
	return
}

func (is *infoschemaV2) AllSchemaNames() []model.CIStr {
	rs := make([]model.CIStr, 0, is.Data.schemaMap.Len())
	is.allSchemas(func(di *model.DBInfo) {
		rs = append(rs, di.Name)
	})
	return rs
}

func (is *infoschemaV2) SchemaExists(schema model.CIStr) bool {
	_, ok := is.SchemaByName(schema)
	return ok
}

func (is *infoschemaV2) FindTableByPartitionID(partitionID int64) (table.Table, *model.DBInfo, *model.PartitionDefinition) {
	var ok bool
	var pi partitionItem
	is.pid2tid.Descend(partitionItem{partitionID: partitionID, schemaVersion: math.MaxInt64},
		func(item partitionItem) bool {
			if item.partitionID != partitionID {
				return false
			}
			if item.schemaVersion > is.infoSchema.schemaMetaVersion {
				// Skip the record.
				return true
			}
			if item.schemaVersion <= is.infoSchema.schemaMetaVersion {
				ok = !item.tomb
				pi = item
				return false
			}
			return true
		})
	if !ok {
		return nil, nil, nil
	}

	tbl, ok := is.TableByID(pi.tableID)
	if !ok {
		// something wrong?
		return nil, nil, nil
	}

	dbID := tbl.Meta().DBID
	dbInfo, ok := is.SchemaByID(dbID)
	if !ok {
		// something wrong?
		return nil, nil, nil
	}

	partInfo := tbl.Meta().GetPartitionInfo()
	var def *model.PartitionDefinition
	for i := 0; i < len(partInfo.Definitions); i++ {
		pdef := &partInfo.Definitions[i]
		if pdef.ID == partitionID {
			def = pdef
			break
		}
	}

	return tbl, dbInfo, def
}

func (is *infoschemaV2) TableExists(schema, table model.CIStr) bool {
	_, err := is.TableByName(context.Background(), schema, table)
	return err == nil
}

func (is *infoschemaV2) SchemaByID(id int64) (*model.DBInfo, bool) {
	if isTableVirtual(id) {
		var st *schemaTables
		is.Data.specials.Range(func(key, value any) bool {
			tmp := value.(*schemaTables)
			if tmp.dbInfo.ID == id {
				st = tmp
				return false
			}
			return true
		})
		if st == nil {
			return nil, false
		}
		return st.dbInfo, true
	}
	var ok bool
	var name string
	is.Data.schemaID2Name.Descend(schemaIDName{
		id:            id,
		schemaVersion: math.MaxInt64,
	}, func(item schemaIDName) bool {
		if item.id != id {
			ok = false
			return false
		}
		if item.schemaVersion <= is.infoSchema.schemaMetaVersion {
			if !item.tomb { // If the item is a tomb record, the database is dropped.
				ok = true
				name = item.name
			}
			return false
		}
		return true
	})
	if !ok {
		return nil, false
	}
	return is.SchemaByName(model.NewCIStr(name))
}

func (is *infoschemaV2) SchemaTables(schema model.CIStr) (tables []table.Table) {
	if IsSpecialDB(schema.L) {
		raw, ok := is.Data.specials.Load(schema.L)
		if ok {
			schTbls := raw.(*schemaTables)
			tables := make([]table.Table, 0, len(schTbls.tables))
			for _, tbl := range schTbls.tables {
				tables = append(tables, tbl)
			}
			return tables
		}
	}

retry:
	dbInfo, ok := is.SchemaByName(schema)
	if !ok {
		return
	}
	snapshot := is.r.Store().GetSnapshot(kv.NewVersion(is.ts))
	// Using the KV timeout read feature to address the issue of potential DDL lease expiration when
	// the meta region leader is slow.
	snapshot.SetOption(kv.TiKVClientReadTimeout, uint64(3000)) // 3000ms.
	m := meta.NewSnapshotMeta(snapshot)
	tblInfos, err := m.ListSimpleTables(dbInfo.ID)
	if err != nil {
		if meta.ErrDBNotExists.Equal(err) {
			return nil
		}
		// Flashback statement could cause such kind of error.
		// In theory that error should be handled in the lower layer, like client-go.
		// But it's not done, so we retry here.
		if strings.Contains(err.Error(), "in flashback progress") {
			time.Sleep(200 * time.Millisecond)
			goto retry
		}
		// TODO: error could happen, so do not panic!
		panic(err)
	}

	tables = make([]table.Table, 0, len(tblInfos))
	for _, tblInfo := range tblInfos {
		tbl, ok := is.tableByID(tblInfo.ID, true)
		if !ok {
			// what happen?
			continue
		}
		tables = append(tables, tbl)
	}
	return
}

func loadTableInfo(ctx context.Context, r autoid.Requirement, infoData *Data, tblID, dbID int64, ts uint64, schemaVersion int64) (table.Table, error) {
	defer tracing.StartRegion(ctx, "infoschema.loadTableInfo").End()
	failpoint.Inject("mockLoadTableInfoError", func(_ failpoint.Value) {
		failpoint.Return(nil, errors.New("mockLoadTableInfoError"))
	})
	// Try to avoid repeated concurrency loading.
	res, err, _ := loadTableSF.Do(fmt.Sprintf("%d-%d-%d", dbID, tblID, schemaVersion), func() (any, error) {
	retry:
		snapshot := r.Store().GetSnapshot(kv.NewVersion(ts))
		// Using the KV timeout read feature to address the issue of potential DDL lease expiration when
		// the meta region leader is slow.
		snapshot.SetOption(kv.TiKVClientReadTimeout, uint64(3000)) // 3000ms.
		m := meta.NewSnapshotMeta(snapshot)

		tblInfo, err := m.GetTable(dbID, tblID)
		if err != nil {
			// Flashback statement could cause such kind of error.
			// In theory that error should be handled in the lower layer, like client-go.
			// But it's not done, so we retry here.
			if strings.Contains(err.Error(), "in flashback progress") {
				time.Sleep(200 * time.Millisecond)
				goto retry
			}

			// TODO load table panic!!!
			panic(err)
		}

		// table removed.
		if tblInfo == nil {
			return nil, errors.Trace(ErrTableNotExists.FastGenByArgs(
				fmt.Sprintf("(Schema ID %d)", dbID),
				fmt.Sprintf("(Table ID %d)", tblID),
			))
		}

		ConvertCharsetCollateToLowerCaseIfNeed(tblInfo)
		ConvertOldVersionUTF8ToUTF8MB4IfNeed(tblInfo)
		allocs := autoid.NewAllocatorsFromTblInfo(r, dbID, tblInfo)
		// TODO: handle cached table!!!
		ret, err := tables.TableFromMeta(allocs, tblInfo)
		if err != nil {
			return nil, errors.Trace(err)
		}
		return ret, err
	})

	if err != nil {
		return nil, errors.Trace(err)
	}
	if res == nil {
		return nil, errors.Trace(ErrTableNotExists.FastGenByArgs(
			fmt.Sprintf("(Schema ID %d)", dbID),
			fmt.Sprintf("(Table ID %d)", tblID),
		))
	}
	return res.(table.Table), nil
}

var loadTableSF = &singleflight.Group{}

func isTableVirtual(id int64) bool {
	// some kind of magic number...
	// we use special ids for tables in INFORMATION_SCHEMA/PERFORMANCE_SCHEMA/METRICS_SCHEMA
	// See meta/autoid/autoid.go for those definitions.
	return (id & autoid.SystemSchemaIDFlag) > 0
}

// IsV2 tells whether an InfoSchema is v2 or not.
func IsV2(is InfoSchema) (bool, *infoschemaV2) {
	ret, ok := is.(*infoschemaV2)
	return ok, ret
}

func applyTableUpdate(b *Builder, m *meta.Meta, diff *model.SchemaDiff) ([]int64, error) {
	if b.enableV2 {
		return b.applyTableUpdateV2(m, diff)
	}
	return b.applyTableUpdate(m, diff)
}

func applyCreateSchema(b *Builder, m *meta.Meta, diff *model.SchemaDiff) error {
	return b.applyCreateSchema(m, diff)
}

func applyDropSchema(b *Builder, diff *model.SchemaDiff) []int64 {
	if b.enableV2 {
		return b.applyDropSchemaV2(diff)
	}
	return b.applyDropSchema(diff)
}

func applyRecoverSchema(b *Builder, m *meta.Meta, diff *model.SchemaDiff) ([]int64, error) {
	if b.enableV2 {
		return b.applyRecoverSchemaV2(m, diff)
	}
	return b.applyRecoverSchema(m, diff)
}

func (b *Builder) applyRecoverSchemaV2(m *meta.Meta, diff *model.SchemaDiff) ([]int64, error) {
	if di, ok := b.infoschemaV2.SchemaByID(diff.SchemaID); ok {
		return nil, ErrDatabaseExists.GenWithStackByArgs(
			fmt.Sprintf("(Schema ID %d)", di.ID),
		)
	}
	di, err := m.GetDatabase(diff.SchemaID)
	if err != nil {
		return nil, errors.Trace(err)
	}
	b.infoschemaV2.addDB(diff.Version, di)
	return applyCreateTables(b, m, diff)
}

func applyModifySchemaCharsetAndCollate(b *Builder, m *meta.Meta, diff *model.SchemaDiff) error {
	if b.enableV2 {
		return b.applyModifySchemaCharsetAndCollateV2(m, diff)
	}
	return b.applyModifySchemaCharsetAndCollate(m, diff)
}

func applyModifySchemaDefaultPlacement(b *Builder, m *meta.Meta, diff *model.SchemaDiff) error {
	if b.enableV2 {
		return b.applyModifySchemaDefaultPlacementV2(m, diff)
	}
	return b.applyModifySchemaDefaultPlacement(m, diff)
}

func applyDropTable(b *Builder, diff *model.SchemaDiff, dbInfo *model.DBInfo, tableID int64, affected []int64) []int64 {
	if b.enableV2 {
		return b.applyDropTableV2(diff, dbInfo, tableID, affected)
	}
	return b.applyDropTable(diff, dbInfo, tableID, affected)
}

func applyCreateTables(b *Builder, m *meta.Meta, diff *model.SchemaDiff) ([]int64, error) {
	return b.applyCreateTables(m, diff)
}

func updateInfoSchemaBundles(b *Builder) {
	if b.enableV2 {
		b.updateInfoSchemaBundlesV2(&b.infoschemaV2)
	} else {
		b.updateInfoSchemaBundles(b.infoSchema)
	}
}

func oldSchemaInfo(b *Builder, diff *model.SchemaDiff) (*model.DBInfo, bool) {
	if b.enableV2 {
		return b.infoschemaV2.SchemaByID(diff.OldSchemaID)
	}

	oldRoDBInfo, ok := b.infoSchema.SchemaByID(diff.OldSchemaID)
	if ok {
		oldRoDBInfo = b.getSchemaAndCopyIfNecessary(oldRoDBInfo.Name.L)
	}
	return oldRoDBInfo, ok
}

// allocByID returns the Allocators of a table.
func allocByID(b *Builder, id int64) (autoid.Allocators, bool) {
	var is InfoSchema
	if b.enableV2 {
		is = &b.infoschemaV2
	} else {
		is = b.infoSchema
	}
	tbl, ok := is.TableByID(id)
	if !ok {
		return autoid.Allocators{}, false
	}
	return tbl.Allocators(nil), true
}

// TODO: more UT to check the correctness.
func (b *Builder) applyTableUpdateV2(m *meta.Meta, diff *model.SchemaDiff) ([]int64, error) {
	oldDBInfo, ok := b.infoschemaV2.SchemaByID(diff.SchemaID)
	if !ok {
		return nil, ErrDatabaseNotExists.GenWithStackByArgs(
			fmt.Sprintf("(Schema ID %d)", diff.SchemaID),
		)
	}

	oldTableID, newTableID := b.getTableIDs(diff)
	b.updateBundleForTableUpdate(diff, newTableID, oldTableID)

	tblIDs, allocs, err := dropTableForUpdate(b, newTableID, oldTableID, oldDBInfo, diff)
	if err != nil {
		return nil, err
	}

	if tableIDIsValid(newTableID) {
		// All types except DropTableOrView.
		var err error
		tblIDs, err = applyCreateTable(b, m, oldDBInfo, newTableID, allocs, diff.Type, tblIDs, diff.Version)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}
	return tblIDs, nil
}

func (b *Builder) applyDropSchemaV2(diff *model.SchemaDiff) []int64 {
	di, ok := b.infoschemaV2.SchemaByID(diff.SchemaID)
	if !ok {
		return nil
	}

	tableIDs := make([]int64, 0, len(di.Tables))
	tables := b.infoschemaV2.SchemaTables(di.Name)
	for _, table := range tables {
		tbl := table.Meta()
		tableIDs = appendAffectedIDs(tableIDs, tbl)
	}

	for _, id := range tableIDs {
		b.deleteBundle(b.infoSchema, id)
		b.applyDropTableV2(diff, di, id, nil)
	}
	b.infoData.deleteDB(di, diff.Version)
	return tableIDs
}

func (b *Builder) applyDropTableV2(diff *model.SchemaDiff, dbInfo *model.DBInfo, tableID int64, affected []int64) []int64 {
	// Remove the table in temporaryTables
	if b.infoSchemaMisc.temporaryTableIDs != nil {
		delete(b.infoSchemaMisc.temporaryTableIDs, tableID)
	}

	table, ok := b.infoschemaV2.TableByID(tableID)
	if !ok {
		return nil
	}

	// The old DBInfo still holds a reference to old table info, we need to remove it.
	b.infoSchema.deleteReferredForeignKeys(dbInfo.Name, table.Meta())

	if pi := table.Meta().GetPartitionInfo(); pi != nil {
		for _, def := range pi.Definitions {
			b.infoData.pid2tid.Set(partitionItem{def.ID, diff.Version, table.Meta().ID, true})
		}
	}

	b.infoData.remove(tableItem{
		dbName:        dbInfo.Name.L,
		dbID:          dbInfo.ID,
		tableName:     table.Meta().Name.L,
		tableID:       table.Meta().ID,
		schemaVersion: diff.Version,
	})

	return affected
}

func (b *Builder) applyModifySchemaCharsetAndCollateV2(m *meta.Meta, diff *model.SchemaDiff) error {
	di, err := m.GetDatabase(diff.SchemaID)
	if err != nil {
		return errors.Trace(err)
	}
	if di == nil {
		// This should never happen.
		return ErrDatabaseNotExists.GenWithStackByArgs(
			fmt.Sprintf("(Schema ID %d)", diff.SchemaID),
		)
	}
	newDBInfo, _ := b.infoschemaV2.SchemaByID(diff.SchemaID)
	newDBInfo.Charset = di.Charset
	newDBInfo.Collate = di.Collate
	b.infoschemaV2.deleteDB(di, diff.Version)
	b.infoschemaV2.addDB(diff.Version, newDBInfo)
	return nil
}

func (b *Builder) applyModifySchemaDefaultPlacementV2(m *meta.Meta, diff *model.SchemaDiff) error {
	di, err := m.GetDatabase(diff.SchemaID)
	if err != nil {
		return errors.Trace(err)
	}
	if di == nil {
		// This should never happen.
		return ErrDatabaseNotExists.GenWithStackByArgs(
			fmt.Sprintf("(Schema ID %d)", diff.SchemaID),
		)
	}
	newDBInfo, _ := b.infoschemaV2.SchemaByID(diff.SchemaID)
	newDBInfo.PlacementPolicyRef = di.PlacementPolicyRef
	b.infoschemaV2.deleteDB(di, diff.Version)
	b.infoschemaV2.addDB(diff.Version, newDBInfo)
	return nil
}

func (b *bundleInfoBuilder) updateInfoSchemaBundlesV2(is *infoschemaV2) {
	if b.deltaUpdate {
		b.completeUpdateTablesV2(is)
		for tblID := range b.updateTables {
			b.updateTableBundles(is, tblID)
		}
		return
	}

	// do full update bundles
	is.ruleBundleMap = make(map[int64]*placement.Bundle)
	tmp := is.ListTablesWithSpecialAttribute(PlacementPolicyAttribute)
	for _, v := range tmp {
		for _, tbl := range v.TableInfos {
			b.updateTableBundles(is, tbl.ID)
		}
	}
}

func (b *bundleInfoBuilder) completeUpdateTablesV2(is *infoschemaV2) {
	if len(b.updatePolicies) == 0 && len(b.updatePartitions) == 0 {
		return
	}

	dbs := is.ListTablesWithSpecialAttribute(AllSpecialAttribute)
	for _, db := range dbs {
		for _, tbl := range db.TableInfos {
			tblInfo := tbl
			if tblInfo.PlacementPolicyRef != nil {
				if _, ok := b.updatePolicies[tblInfo.PlacementPolicyRef.ID]; ok {
					b.markTableBundleShouldUpdate(tblInfo.ID)
				}
			}

			if tblInfo.Partition != nil {
				for _, par := range tblInfo.Partition.Definitions {
					if _, ok := b.updatePartitions[par.ID]; ok {
						b.markTableBundleShouldUpdate(tblInfo.ID)
					}
				}
			}
		}
	}
}

type specialAttributeFilter func(*model.TableInfo) bool

// TTLAttribute is the TTL attribute filter used by ListTablesWithSpecialAttribute.
var TTLAttribute specialAttributeFilter = func(t *model.TableInfo) bool {
	return t.State == model.StatePublic && t.TTLInfo != nil
}

// TiFlashAttribute is the TiFlashReplica attribute filter used by ListTablesWithSpecialAttribute.
var TiFlashAttribute specialAttributeFilter = func(t *model.TableInfo) bool {
	return t.TiFlashReplica != nil
}

// PlacementPolicyAttribute is the Placement Policy attribute filter used by ListTablesWithSpecialAttribute.
var PlacementPolicyAttribute specialAttributeFilter = func(t *model.TableInfo) bool {
	if t.PlacementPolicyRef != nil {
		return true
	}
	if parInfo := t.GetPartitionInfo(); parInfo != nil {
		for _, def := range parInfo.Definitions {
			if def.PlacementPolicyRef != nil {
				return true
			}
		}
	}
	return false
}

// PartitionAttribute is the Partition attribute filter used by ListTablesWithSpecialAttribute.
var PartitionAttribute specialAttributeFilter = func(t *model.TableInfo) bool {
	return t.GetPartitionInfo() != nil
}

func hasSpecialAttributes(t *model.TableInfo) bool {
	return TTLAttribute(t) || TiFlashAttribute(t) || PlacementPolicyAttribute(t) || PartitionAttribute(t)
}

// AllSpecialAttribute marks a model.TableInfo with any special attributes.
var AllSpecialAttribute specialAttributeFilter = hasSpecialAttributes

func (is *infoschemaV2) ListTablesWithSpecialAttribute(filter specialAttributeFilter) []tableInfoResult {
	ret := make([]tableInfoResult, 0, 10)
	var currDB string
	var lastTableID int64
	var res tableInfoResult
	is.Data.tableInfoResident.Reverse(func(item tableInfoItem) bool {
		if item.schemaVersion > is.infoSchema.schemaMetaVersion {
			// Skip the versions that we are not looking for.
			return true
		}
		// Dedup the same record of different versions.
		if lastTableID != 0 && lastTableID == item.tableID {
			return true
		}
		lastTableID = item.tableID

		if item.tomb {
			return true
		}

		if !filter(item.tableInfo) {
			return true
		}

		if currDB == "" {
			currDB = item.dbName
			res = tableInfoResult{DBName: item.dbName}
			res.TableInfos = append(res.TableInfos, item.tableInfo)
		} else if currDB == item.dbName {
			res.TableInfos = append(res.TableInfos, item.tableInfo)
		} else {
			ret = append(ret, res)
			res = tableInfoResult{DBName: item.dbName}
			res.TableInfos = append(res.TableInfos, item.tableInfo)
		}
		return true
	})
	if len(res.TableInfos) > 0 {
		ret = append(ret, res)
	}
	return ret
}
