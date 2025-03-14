// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package export

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pingcap/dumpling/v4/cli"
	tcontext "github.com/pingcap/dumpling/v4/context"
	"github.com/pingcap/dumpling/v4/log"

	// import mysql driver
	_ "github.com/go-sql-driver/mysql"
	"github.com/pingcap/br/pkg/storage"
	"github.com/pingcap/br/pkg/summary"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	pclog "github.com/pingcap/log"
	"github.com/pingcap/tidb/store/helper"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/util/codec"
	pd "github.com/tikv/pd/client"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

var openDBFunc = sql.Open

// Dumper is the dump progress structure
type Dumper struct {
	tctx      *tcontext.Context
	conf      *Config
	cancelCtx context.CancelFunc

	extStore storage.ExternalStorage
	dbHandle *sql.DB

	tidbPDClientForGC         pd.Client
	selectTiDBTableRegionFunc func(tctx *tcontext.Context, conn *sql.Conn, dbName, tableName string) (pkFields []string, pkVals [][]string, err error)
}

// NewDumper returns a new Dumper
func NewDumper(ctx context.Context, conf *Config) (*Dumper, error) {
	tctx, cancelFn := tcontext.Background().WithContext(ctx).WithCancel()
	d := &Dumper{
		tctx:                      tctx,
		conf:                      conf,
		cancelCtx:                 cancelFn,
		selectTiDBTableRegionFunc: selectTiDBTableRegion,
	}
	err := adjustConfig(conf,
		registerTLSConfig,
		validateSpecifiedSQL,
		adjustFileFormat)
	if err != nil {
		return nil, err
	}
	err = runSteps(d,
		initLogger,
		createExternalStore,
		startHTTPService,
		openSQLDB,
		detectServerInfo,
		resolveAutoConsistency,

		tidbSetPDClientForGC,
		tidbGetSnapshot,
		tidbStartGCSavepointUpdateService,

		setSessionParam)
	return d, err
}

// Dump dumps table from database
// nolint: gocyclo
func (d *Dumper) Dump() (dumpErr error) {
	initColTypeRowReceiverMap()
	var (
		conn    *sql.Conn
		err     error
		conCtrl ConsistencyController
	)
	tctx, conf, pool := d.tctx, d.conf, d.dbHandle
	tctx.L().Info("begin to run Dump", zap.Stringer("conf", conf))
	m := newGlobalMetadata(tctx, d.extStore, conf.Snapshot)
	defer func() {
		if dumpErr == nil {
			_ = m.writeGlobalMetaData()
		}
	}()

	// for consistency lock, we should get table list at first to generate the lock tables SQL
	if conf.Consistency == consistencyTypeLock {
		conn, err = createConnWithConsistency(tctx, pool)
		if err != nil {
			return errors.Trace(err)
		}
		if err = prepareTableListToDump(tctx, conf, conn); err != nil {
			conn.Close()
			return err
		}
		conn.Close()
	}

	conCtrl, err = NewConsistencyController(tctx, conf, pool)
	if err != nil {
		return err
	}
	if err = conCtrl.Setup(tctx); err != nil {
		return errors.Trace(err)
	}
	// To avoid lock is not released
	defer func() {
		err = conCtrl.TearDown(tctx)
		if err != nil {
			tctx.L().Error("fail to tear down consistency controller", zap.Error(err))
		}
	}()

	metaConn, err := createConnWithConsistency(tctx, pool)
	if err != nil {
		return err
	}
	defer metaConn.Close()
	m.recordStartTime(time.Now())
	// for consistency lock, we can write snapshot info after all tables are locked.
	// the binlog pos may changed because there is still possible write between we lock tables and write master status.
	// but for the locked tables doing replication that starts from metadata is safe.
	// for consistency flush, record snapshot after whole tables are locked. The recorded meta info is exactly the locked snapshot.
	// for consistency snapshot, we should use the snapshot that we get/set at first in metadata. TiDB will assure the snapshot of TSO.
	// for consistency none, the binlog pos in metadata might be earlier than dumped data. We need to enable safe-mode to assure data safety.
	err = m.recordGlobalMetaData(metaConn, conf.ServerInfo.ServerType, false)
	if err != nil {
		tctx.L().Info("get global metadata failed", zap.Error(err))
	}

	// for other consistencies, we should get table list after consistency is set up and GlobalMetaData is cached
	if conf.Consistency != consistencyTypeLock {
		if err = prepareTableListToDump(tctx, conf, metaConn); err != nil {
			return err
		}
	}
	if err = d.renewSelectTableRegionFuncForLowerTiDB(tctx); err != nil {
		tctx.L().Error("fail to update select table region info for TiDB", zap.Error(err))
	}

	rebuildConn := func(conn *sql.Conn) (*sql.Conn, error) {
		// make sure that the lock connection is still alive
		err1 := conCtrl.PingContext(tctx)
		if err1 != nil {
			return conn, errors.Trace(err1)
		}
		// give up the last broken connection
		conn.Close()
		newConn, err1 := createConnWithConsistency(tctx, pool)
		if err1 != nil {
			return conn, errors.Trace(err1)
		}
		conn = newConn
		// renew the master status after connection. dm can't close safe-mode until dm reaches current pos
		if conf.PosAfterConnect {
			err1 = m.recordGlobalMetaData(conn, conf.ServerInfo.ServerType, true)
			if err1 != nil {
				return conn, errors.Trace(err1)
			}
		}
		return conn, nil
	}

	taskChan := make(chan Task, defaultDumpThreads)
	AddGauge(taskChannelCapacity, conf.Labels, defaultDumpThreads)
	wg, writingCtx := errgroup.WithContext(tctx)
	writerCtx := tctx.WithContext(writingCtx)
	writers, tearDownWriters, err := d.startWriters(writerCtx, wg, taskChan, rebuildConn)
	if err != nil {
		return err
	}
	defer tearDownWriters()

	if conf.TransactionalConsistency {
		if conf.Consistency == consistencyTypeFlush || conf.Consistency == consistencyTypeLock {
			tctx.L().Info("All the dumping transactions have started. Start to unlock tables")
		}
		if err = conCtrl.TearDown(tctx); err != nil {
			return errors.Trace(err)
		}
	}
	// Inject consistency failpoint test after we release the table lock
	failpoint.Inject("ConsistencyCheck", nil)

	if conf.PosAfterConnect {
		// record again, to provide a location to exit safe mode for DM
		err = m.recordGlobalMetaData(metaConn, conf.ServerInfo.ServerType, true)
		if err != nil {
			tctx.L().Info("get global metadata (after connection pool established) failed", zap.Error(err))
		}
	}

	summary.SetLogCollector(summary.NewLogCollector(tctx.L().Info))
	summary.SetUnit(summary.BackupUnit)
	defer summary.Summary(summary.BackupUnit)

	logProgressCtx, logProgressCancel := tctx.WithCancel()
	go d.runLogProgress(logProgressCtx)
	defer logProgressCancel()

	tableDataStartTime := time.Now()

	failpoint.Inject("PrintTiDBMemQuotaQuery", func(_ failpoint.Value) {
		row := d.dbHandle.QueryRowContext(tctx, "select @@tidb_mem_quota_query;")
		var s string
		err = row.Scan(&s)
		if err != nil {
			fmt.Println(errors.Trace(err))
		} else {
			fmt.Printf("tidb_mem_quota_query == %s\n", s)
		}
	})

	// get estimate total count
	if err = d.getEstimateTotalRowsCount(tctx, metaConn); err != nil {
		tctx.L().Error("fail to get estimate total count", zap.Error(err))
	}

	if conf.SQL == "" {
		if err = d.dumpDatabases(writerCtx, metaConn, taskChan); err != nil && !errors.ErrorEqual(err, context.Canceled) {
			return err
		}
	} else {
		d.dumpSQL(writerCtx, taskChan)
	}
	close(taskChan)
	if err := wg.Wait(); err != nil {
		summary.CollectFailureUnit("dump table data", err)
		return errors.Trace(err)
	}
	summary.CollectSuccessUnit("dump cost", countTotalTask(writers), time.Since(tableDataStartTime))

	summary.SetSuccessStatus(true)
	m.recordFinishTime(time.Now())
	return nil
}

func (d *Dumper) startWriters(tctx *tcontext.Context, wg *errgroup.Group, taskChan <-chan Task,
	rebuildConnFn func(*sql.Conn) (*sql.Conn, error)) ([]*Writer, func(), error) {
	conf, pool := d.conf, d.dbHandle
	writers := make([]*Writer, conf.Threads)
	for i := 0; i < conf.Threads; i++ {
		conn, err := createConnWithConsistency(tctx, pool)
		if err != nil {
			return nil, func() {}, err
		}
		writer := NewWriter(tctx, int64(i), conf, conn, d.extStore)
		writer.rebuildConnFn = rebuildConnFn
		writer.setFinishTableCallBack(func(task Task) {
			if _, ok := task.(*TaskTableData); ok {
				IncCounter(finishedTablesCounter, conf.Labels)
				// FIXME: actually finishing the last chunk doesn't means this table is 'finished'.
				//  We can call this table is 'finished' if all its chunks are finished.
				//  Comment this log now to avoid ambiguity.
				// tctx.L().Debug("finished dumping table data",
				//	zap.String("database", td.Meta.DatabaseName()),
				//	zap.String("table", td.Meta.TableName()))
			}
		})
		writer.setFinishTaskCallBack(func(task Task) {
			IncGauge(taskChannelCapacity, conf.Labels)
			if td, ok := task.(*TaskTableData); ok {
				tctx.L().Debug("finish dumping table data task",
					zap.String("database", td.Meta.DatabaseName()),
					zap.String("table", td.Meta.TableName()),
					zap.Int("chunkIdx", td.ChunkIndex))
			}
		})
		wg.Go(func() error {
			return writer.run(taskChan)
		})
		writers[i] = writer
	}
	tearDown := func() {
		for _, w := range writers {
			w.conn.Close()
		}
	}
	return writers, tearDown, nil
}

func (d *Dumper) dumpDatabases(tctx *tcontext.Context, metaConn *sql.Conn, taskChan chan<- Task) error {
	conf := d.conf
	allTables := conf.Tables
	for dbName, tables := range allTables {
		createDatabaseSQL, err := ShowCreateDatabase(metaConn, dbName)
		if err != nil {
			return err
		}
		task := NewTaskDatabaseMeta(dbName, createDatabaseSQL)
		ctxDone := d.sendTaskToChan(tctx, task, taskChan)
		if ctxDone {
			return tctx.Err()
		}

		for _, table := range tables {
			tctx.L().Debug("start dumping table...", zap.String("database", dbName),
				zap.String("table", table.Name))
			meta, err := dumpTableMeta(conf, metaConn, dbName, table)
			if err != nil {
				return err
			}

			if table.Type == TableTypeView {
				task := NewTaskViewMeta(dbName, table.Name, meta.ShowCreateTable(), meta.ShowCreateView())
				ctxDone = d.sendTaskToChan(tctx, task, taskChan)
				if ctxDone {
					return tctx.Err()
				}
			} else {
				task := NewTaskTableMeta(dbName, table.Name, meta.ShowCreateTable())
				ctxDone = d.sendTaskToChan(tctx, task, taskChan)
				if ctxDone {
					return tctx.Err()
				}
				err = d.dumpTableData(tctx, metaConn, meta, taskChan)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (d *Dumper) dumpTableData(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta, taskChan chan<- Task) error {
	conf := d.conf
	if conf.NoData {
		return nil
	}
	if conf.Rows == UnspecifiedSize {
		return d.sequentialDumpTable(tctx, conn, meta, taskChan)
	}
	return d.concurrentDumpTable(tctx, conn, meta, taskChan)
}

func (d *Dumper) buildConcatTask(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta) (*TaskTableData, error) {
	tableChan := make(chan Task, 128)
	errCh := make(chan error, 1)
	go func() {
		// adjust rows to suitable rows for this table
		d.conf.Rows = GetSuitableRows(tctx, conn, meta.DatabaseName(), meta.TableName())
		err := d.concurrentDumpTable(tctx, conn, meta, tableChan)
		d.conf.Rows = UnspecifiedSize
		if err != nil {
			errCh <- err
		} else {
			close(errCh)
		}
	}()
	tableDataArr := make([]*tableData, 0)
	handleSubTask := func(task Task) {
		tableTask, ok := task.(*TaskTableData)
		if !ok {
			tctx.L().Warn("unexpected task when splitting table chunks", zap.String("task", tableTask.Brief()))
			return
		}
		tableDataInst, ok := tableTask.Data.(*tableData)
		if !ok {
			tctx.L().Warn("unexpected task.Data when splitting table chunks", zap.String("task", tableTask.Brief()))
			return
		}
		tableDataArr = append(tableDataArr, tableDataInst)
	}
	for {
		select {
		case err, ok := <-errCh:
			if !ok {
				// make sure all the subtasks in tableChan are handled
				for len(tableChan) > 0 {
					task := <-tableChan
					handleSubTask(task)
				}
				if len(tableDataArr) <= 1 {
					return nil, nil
				}
				queries := make([]string, 0, len(tableDataArr))
				colLen := tableDataArr[0].colLen
				for _, tableDataInst := range tableDataArr {
					queries = append(queries, tableDataInst.query)
					if colLen != tableDataInst.colLen {
						tctx.L().Warn("colLen varies for same table",
							zap.Int("oldColLen", colLen),
							zap.String("oldQuery", queries[0]),
							zap.Int("newColLen", tableDataInst.colLen),
							zap.String("newQuery", tableDataInst.query))
						return nil, nil
					}
				}
				return NewTaskTableData(meta, newMultiQueriesChunk(queries, colLen), 0, 1), nil
			}
			return nil, err
		case task := <-tableChan:
			handleSubTask(task)
		}
	}
}

func (d *Dumper) dumpWholeTableDirectly(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta, taskChan chan<- Task, partition string, currentChunk, totalChunks int) error {
	conf := d.conf
	tableIR, err := SelectAllFromTable(conf, conn, meta, partition)
	if err != nil {
		return err
	}
	task := NewTaskTableData(meta, tableIR, currentChunk, totalChunks)
	ctxDone := d.sendTaskToChan(tctx, task, taskChan)
	if ctxDone {
		return tctx.Err()
	}
	return nil
}

func (d *Dumper) sequentialDumpTable(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta, taskChan chan<- Task) error {
	conf := d.conf
	if conf.ServerInfo.ServerType == ServerTypeTiDB {
		task, err := d.buildConcatTask(tctx, conn, meta)
		if err != nil {
			return errors.Trace(err)
		}
		if task != nil {
			ctxDone := d.sendTaskToChan(tctx, task, taskChan)
			if ctxDone {
				return tctx.Err()
			}
			return nil
		}
		tctx.L().Info("didn't build tidb concat sqls, will select all from table now",
			zap.String("database", meta.DatabaseName()),
			zap.String("table", meta.TableName()))
	}
	return d.dumpWholeTableDirectly(tctx, conn, meta, taskChan, "", 0, 1)
}

// concurrentDumpTable tries to split table into several chunks to dump
func (d *Dumper) concurrentDumpTable(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta, taskChan chan<- Task) error {
	conf := d.conf
	db, tbl := meta.DatabaseName(), meta.TableName()
	if conf.ServerInfo.ServerType == ServerTypeTiDB &&
		conf.ServerInfo.ServerVersion != nil &&
		(conf.ServerInfo.ServerVersion.Compare(*tableSampleVersion) >= 0 ||
			(conf.ServerInfo.HasTiKV && conf.ServerInfo.ServerVersion.Compare(*decodeRegionVersion) >= 0)) {
		return d.concurrentDumpTiDBTables(tctx, conn, meta, taskChan)
	}
	field, err := pickupPossibleField(db, tbl, conn, conf)
	if err != nil {
		return err
	}
	if field == "" {
		// skip split chunk logic if not found proper field
		tctx.L().Warn("fallback to sequential dump due to no proper field",
			zap.String("database", db), zap.String("table", tbl))
		return d.dumpWholeTableDirectly(tctx, conn, meta, taskChan, "", 0, 1)
	}

	min, max, err := d.selectMinAndMaxIntValue(conn, db, tbl, field)
	if err != nil {
		return err
	}
	tctx.L().Debug("get int bounding values",
		zap.String("lower", min.String()),
		zap.String("upper", max.String()))

	count := estimateCount(d.tctx, db, tbl, conn, field, conf)
	tctx.L().Info("get estimated rows count",
		zap.String("database", db),
		zap.String("table", tbl),
		zap.Uint64("estimateCount", count))
	if count < conf.Rows {
		// skip chunk logic if estimates are low
		tctx.L().Warn("skip concurrent dump due to estimate count < rows",
			zap.Uint64("estimate count", count),
			zap.Uint64("conf.rows", conf.Rows),
			zap.String("database", db),
			zap.String("table", tbl))
		return d.dumpWholeTableDirectly(tctx, conn, meta, taskChan, "", 0, 1)
	}

	// every chunk would have eventual adjustments
	estimatedChunks := count / conf.Rows
	estimatedStep := new(big.Int).Sub(max, min).Uint64()/estimatedChunks + 1
	bigEstimatedStep := new(big.Int).SetUint64(estimatedStep)
	cutoff := new(big.Int).Set(min)
	totalChunks := estimatedChunks
	if estimatedStep == 1 {
		totalChunks = new(big.Int).Sub(max, min).Uint64() + 1
	}

	selectField, selectLen, err := buildSelectField(conn, db, tbl, conf.CompleteInsert)
	if err != nil {
		return err
	}

	orderByClause, err := buildOrderByClause(conf, conn, db, tbl)
	if err != nil {
		return err
	}

	chunkIndex := 0
	nullValueCondition := ""
	if conf.Where == "" {
		nullValueCondition = fmt.Sprintf("`%s` IS NULL OR ", escapeString(field))
	}
	for max.Cmp(cutoff) >= 0 {
		nextCutOff := new(big.Int).Add(cutoff, bigEstimatedStep)
		where := fmt.Sprintf("%s(`%s` >= %d AND `%s` < %d)", nullValueCondition, escapeString(field), cutoff, escapeString(field), nextCutOff)
		query := buildSelectQuery(db, tbl, selectField, "", buildWhereCondition(conf, where), orderByClause)
		if len(nullValueCondition) > 0 {
			nullValueCondition = ""
		}
		task := NewTaskTableData(meta, newTableData(query, selectLen, false), chunkIndex, int(totalChunks))
		ctxDone := d.sendTaskToChan(tctx, task, taskChan)
		if ctxDone {
			return tctx.Err()
		}
		cutoff = nextCutOff
		chunkIndex++
	}
	return nil
}

func (d *Dumper) sendTaskToChan(tctx *tcontext.Context, task Task, taskChan chan<- Task) (ctxDone bool) {
	conf := d.conf
	select {
	case <-tctx.Done():
		return true
	case taskChan <- task:
		tctx.L().Debug("send task to writer",
			zap.String("task", task.Brief()))
		DecGauge(taskChannelCapacity, conf.Labels)
		return false
	}
}

func (d *Dumper) selectMinAndMaxIntValue(conn *sql.Conn, db, tbl, field string) (*big.Int, *big.Int, error) {
	tctx, conf, zero := d.tctx, d.conf, &big.Int{}
	query := fmt.Sprintf("SELECT MIN(`%s`),MAX(`%s`) FROM `%s`.`%s`",
		escapeString(field), escapeString(field), escapeString(db), escapeString(tbl))
	if conf.Where != "" {
		query = fmt.Sprintf("%s WHERE %s", query, conf.Where)
	}
	tctx.L().Debug("split chunks", zap.String("query", query))

	var smin sql.NullString
	var smax sql.NullString
	row := conn.QueryRowContext(tctx, query)
	err := row.Scan(&smin, &smax)
	if err != nil {
		tctx.L().Error("split chunks - get max min failed", zap.String("query", query), zap.Error(err))
		return zero, zero, errors.Trace(err)
	}
	if !smax.Valid || !smin.Valid {
		// found no data
		tctx.L().Warn("no data to dump", zap.String("database", db), zap.String("table", tbl))
		return zero, zero, nil
	}

	max := new(big.Int)
	min := new(big.Int)
	var ok bool
	if max, ok = max.SetString(smax.String, 10); !ok {
		return zero, zero, errors.Errorf("fail to convert max value %s in query %s", smax.String, query)
	}
	if min, ok = min.SetString(smin.String, 10); !ok {
		return zero, zero, errors.Errorf("fail to convert min value %s in query %s", smin.String, query)
	}
	return min, max, nil
}

func (d *Dumper) concurrentDumpTiDBTables(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta, taskChan chan<- Task) error {
	db, tbl := meta.DatabaseName(), meta.TableName()

	var (
		handleColNames []string
		handleVals     [][]string
		err            error
	)
	// for TiDB v5.0+, we can use table sample directly
	if d.conf.ServerInfo.ServerVersion.Compare(*tableSampleVersion) >= 0 {
		tctx.L().Debug("dumping TiDB tables with TABLESAMPLE",
			zap.String("database", db), zap.String("table", tbl))
		handleColNames, handleVals, err = selectTiDBTableSample(tctx, conn, db, tbl)
	} else {
		// for TiDB v3.0+, we can use table region decode in TiDB directly
		tctx.L().Debug("dumping TiDB tables with TABLE REGIONS",
			zap.String("database", db), zap.String("table", tbl))
		var partitions []string
		if d.conf.ServerInfo.ServerVersion.Compare(*gcSafePointVersion) >= 0 {
			partitions, err = GetPartitionNames(conn, db, tbl)
		}
		if err == nil {
			if len(partitions) == 0 {
				handleColNames, handleVals, err = d.selectTiDBTableRegionFunc(tctx, conn, db, tbl)
			} else {
				return d.concurrentDumpTiDBPartitionTables(tctx, conn, meta, taskChan, partitions)
			}
		}
	}
	if err != nil {
		return err
	}
	return d.sendConcurrentDumpTiDBTasks(tctx, conn, meta, taskChan, handleColNames, handleVals, "", 0, len(handleVals)+1)
}

func (d *Dumper) concurrentDumpTiDBPartitionTables(tctx *tcontext.Context, conn *sql.Conn, meta TableMeta, taskChan chan<- Task, partitions []string) error {
	db, tbl := meta.DatabaseName(), meta.TableName()
	tctx.L().Debug("dumping TiDB tables with TABLE REGIONS for partition table",
		zap.String("database", db), zap.String("table", tbl), zap.Strings("partitions", partitions))

	startChunkIdx := 0
	totalChunk := 0
	cachedHandleVals := make([][][]string, len(partitions))

	handleColNames, _, err := selectTiDBRowKeyFields(conn, db, tbl, checkTiDBTableRegionPkFields)
	if err != nil {
		return err
	}
	// cache handleVals here to calculate the total chunks
	for i, partition := range partitions {
		handleVals, err := selectTiDBPartitionRegion(tctx, conn, db, tbl, partition)
		if err != nil {
			return err
		}
		totalChunk += len(handleVals) + 1
		cachedHandleVals[i] = handleVals
	}
	for i, partition := range partitions {
		err := d.sendConcurrentDumpTiDBTasks(tctx, conn, meta, taskChan, handleColNames, cachedHandleVals[i], partition, startChunkIdx, totalChunk)
		if err != nil {
			return err
		}
		startChunkIdx += len(cachedHandleVals[i]) + 1
	}
	return nil
}

func (d *Dumper) sendConcurrentDumpTiDBTasks(tctx *tcontext.Context,
	conn *sql.Conn, meta TableMeta, taskChan chan<- Task,
	handleColNames []string, handleVals [][]string, partition string, startChunkIdx, totalChunk int) error {
	if len(handleVals) == 0 {
		return d.dumpWholeTableDirectly(tctx, conn, meta, taskChan, partition, startChunkIdx, totalChunk)
	}
	conf := d.conf
	db, tbl := meta.DatabaseName(), meta.TableName()
	selectField, selectLen, err := buildSelectField(conn, db, tbl, conf.CompleteInsert)
	if err != nil {
		return err
	}
	where := buildWhereClauses(handleColNames, handleVals)
	orderByClause := buildOrderByClauseString(handleColNames)

	for i, w := range where {
		query := buildSelectQuery(db, tbl, selectField, partition, buildWhereCondition(conf, w), orderByClause)
		task := NewTaskTableData(meta, newTableData(query, selectLen, false), i+startChunkIdx, totalChunk)
		ctxDone := d.sendTaskToChan(tctx, task, taskChan)
		if ctxDone {
			return tctx.Err()
		}
	}
	return nil
}

// L returns real logger
func (d *Dumper) L() log.Logger {
	return d.tctx.L()
}

func selectTiDBTableSample(tctx *tcontext.Context, conn *sql.Conn, dbName, tableName string) (pkFields []string, pkVals [][]string, err error) {
	pkFields, pkColTypes, err := selectTiDBRowKeyFields(conn, dbName, tableName, nil)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}

	query := buildTiDBTableSampleQuery(pkFields, dbName, tableName)
	rows, err := conn.QueryContext(tctx, query)
	if err != nil {
		return nil, nil, errors.Trace(err)
	}
	pkValNum := len(pkFields)
	iter := newRowIter(rows, pkValNum)
	defer iter.Close()
	rowRec := MakeRowReceiver(pkColTypes)
	buf := new(bytes.Buffer)

	for iter.HasNext() {
		err = iter.Decode(rowRec)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		pkValRow := make([]string, 0, pkValNum)
		for _, rec := range rowRec.receivers {
			rec.WriteToBuffer(buf, true)
			pkValRow = append(pkValRow, buf.String())
			buf.Reset()
		}
		pkVals = append(pkVals, pkValRow)
		iter.Next()
	}
	iter.Close()
	return pkFields, pkVals, iter.Error()
}

func buildTiDBTableSampleQuery(pkFields []string, dbName, tblName string) string {
	template := "SELECT %s FROM `%s`.`%s` TABLESAMPLE REGIONS() ORDER BY %s"
	quotaPk := make([]string, len(pkFields))
	for i, s := range pkFields {
		quotaPk[i] = fmt.Sprintf("`%s`", escapeString(s))
	}
	pks := strings.Join(quotaPk, ",")
	return fmt.Sprintf(template, pks, escapeString(dbName), escapeString(tblName), pks)
}

func selectTiDBRowKeyFields(conn *sql.Conn, dbName, tableName string, checkPkFields func([]string, []string) error) (pkFields, pkColTypes []string, err error) {
	hasImplicitRowID, err := SelectTiDBRowID(conn, dbName, tableName)
	if err != nil {
		return
	}
	if hasImplicitRowID {
		pkFields, pkColTypes = []string{"_tidb_rowid"}, []string{"BIGINT"}
	} else {
		pkFields, pkColTypes, err = GetPrimaryKeyAndColumnTypes(conn, dbName, tableName)
		if err == nil {
			if checkPkFields != nil {
				err = checkPkFields(pkFields, pkColTypes)
			}
		}
	}
	return
}

func checkTiDBTableRegionPkFields(pkFields, pkColTypes []string) (err error) {
	if len(pkFields) != 1 || len(pkColTypes) != 1 {
		err = errors.Errorf("unsupported primary key for selectTableRegion. pkFields: [%s], pkColTypes: [%s]", strings.Join(pkFields, ", "), strings.Join(pkColTypes, ", "))
		return
	}
	if _, ok := dataTypeNum[pkColTypes[0]]; !ok {
		err = errors.Errorf("unsupported primary key type for selectTableRegion. pkFields: [%s], pkColTypes: [%s]", strings.Join(pkFields, ", "), strings.Join(pkColTypes, ", "))
	}
	return
}

func selectTiDBTableRegion(tctx *tcontext.Context, conn *sql.Conn, dbName, tableName string) (pkFields []string, pkVals [][]string, err error) {
	pkFields, _, err = selectTiDBRowKeyFields(conn, dbName, tableName, checkTiDBTableRegionPkFields)
	if err != nil {
		return
	}

	var (
		startKey, decodedKey sql.NullString
		rowID                = -1
	)
	const (
		tableRegionSQL = "SELECT START_KEY,tidb_decode_key(START_KEY) from INFORMATION_SCHEMA.TIKV_REGION_STATUS s WHERE s.DB_NAME = ? AND s.TABLE_NAME = ? AND IS_INDEX = 0 ORDER BY START_KEY;"
		tidbRowID      = "_tidb_rowid="
	)
	logger := tctx.L().With(zap.String("database", dbName), zap.String("table", tableName))
	err = simpleQueryWithArgs(conn, func(rows *sql.Rows) error {
		rowID++
		err = rows.Scan(&startKey, &decodedKey)
		if err != nil {
			return errors.Trace(err)
		}
		// first region's start key has no use. It may come from another table or might be invalid
		if rowID == 0 {
			return nil
		}
		if !startKey.Valid {
			logger.Debug("meet invalid start key", zap.Int("rowID", rowID))
			return nil
		}
		if !decodedKey.Valid {
			logger.Debug("meet invalid decoded start key", zap.Int("rowID", rowID), zap.String("startKey", startKey.String))
			return nil
		}
		pkVal, err2 := extractTiDBRowIDFromDecodedKey(tidbRowID, decodedKey.String)
		if err2 != nil {
			logger.Debug("fail to extract pkVal from decoded start key",
				zap.Int("rowID", rowID), zap.String("startKey", startKey.String), zap.String("decodedKey", decodedKey.String), zap.Error(err2))
		} else {
			pkVals = append(pkVals, []string{pkVal})
		}
		return nil
	}, tableRegionSQL, dbName, tableName)

	return pkFields, pkVals, errors.Trace(err)
}

func selectTiDBPartitionRegion(tctx *tcontext.Context, conn *sql.Conn, dbName, tableName, partition string) (pkVals [][]string, err error) {
	var (
		rows      *sql.Rows
		startKeys []string
	)
	const (
		partitionRegionSQL = "SHOW TABLE `%s`.`%s` PARTITION(`%s`) REGIONS"
		regionRowKey       = "r_"
	)
	logger := tctx.L().With(zap.String("database", dbName), zap.String("table", tableName), zap.String("partition", partition))
	rows, err = conn.QueryContext(tctx, fmt.Sprintf(partitionRegionSQL, escapeString(dbName), escapeString(tableName), escapeString(partition)))
	if err != nil {
		err = errors.Trace(err)
		return
	}
	startKeys, err = GetSpecifiedColumnValueAndClose(rows, "START_KEY")
	if err != nil {
		return
	}
	for rowID, startKey := range startKeys {
		if rowID == 0 {
			continue
		}
		pkVal, err2 := extractTiDBRowIDFromDecodedKey(regionRowKey, startKey)
		if err2 != nil {
			logger.Debug("show table region start key doesn't have rowID",
				zap.Int("rowID", rowID), zap.String("startKey", startKey), zap.Error(err2))
		} else {
			pkVals = append(pkVals, []string{pkVal})
		}
	}

	return pkVals, err
}

func extractTiDBRowIDFromDecodedKey(indexField, key string) (string, error) {
	if p := strings.Index(key, indexField); p != -1 {
		p += len(indexField)
		return key[p:], nil
	}
	return "", errors.Errorf("decoded key %s doesn't have %s field", key, indexField)
}

func prepareTableListToDump(tctx *tcontext.Context, conf *Config, db *sql.Conn) error {
	databases, err := prepareDumpingDatabases(conf, db)
	if err != nil {
		return err
	}

	conf.Tables, err = listAllTables(db, databases)
	if err != nil {
		return err
	}

	if !conf.NoViews {
		views, err := listAllViews(db, databases)
		if err != nil {
			return err
		}
		conf.Tables.Merge(views)
	}

	filterTables(tctx, conf)
	return nil
}

func dumpTableMeta(conf *Config, conn *sql.Conn, db string, table *TableInfo) (TableMeta, error) {
	tbl := table.Name
	selectField, _, err := buildSelectField(conn, db, tbl, conf.CompleteInsert)
	if err != nil {
		return nil, err
	}

	var colTypes []*sql.ColumnType
	// If all columns are generated
	if selectField == "" {
		colTypes, err = GetColumnTypes(conn, "*", db, tbl)
	} else {
		colTypes, err = GetColumnTypes(conn, selectField, db, tbl)
	}
	if err != nil {
		return nil, err
	}

	meta := &tableMeta{
		database:      db,
		table:         tbl,
		colTypes:      colTypes,
		selectedField: selectField,
		specCmts: []string{
			"/*!40101 SET NAMES binary*/;",
		},
	}

	if conf.NoSchemas {
		return meta, nil
	}
	if table.Type == TableTypeView {
		viewName := table.Name
		createTableSQL, createViewSQL, err1 := ShowCreateView(conn, db, viewName)
		if err1 != nil {
			return meta, err1
		}
		meta.showCreateTable = createTableSQL
		meta.showCreateView = createViewSQL
		return meta, nil
	}
	createTableSQL, err := ShowCreateTable(conn, db, tbl)
	if err != nil {
		return nil, err
	}
	meta.showCreateTable = createTableSQL
	return meta, nil
}

func (d *Dumper) dumpSQL(tctx *tcontext.Context, taskChan chan<- Task) {
	conf := d.conf
	meta := &tableMeta{}
	data := newTableData(conf.SQL, 0, true)
	task := NewTaskTableData(meta, data, 0, 1)
	d.sendTaskToChan(tctx, task, taskChan)
}

func canRebuildConn(consistency string, trxConsistencyOnly bool) bool {
	switch consistency {
	case consistencyTypeLock, consistencyTypeFlush:
		return !trxConsistencyOnly
	case consistencyTypeSnapshot, consistencyTypeNone:
		return true
	default:
		return false
	}
}

// Close closes a Dumper and stop dumping immediately
func (d *Dumper) Close() error {
	d.cancelCtx()
	return d.dbHandle.Close()
}

func runSteps(d *Dumper, steps ...func(*Dumper) error) error {
	for _, st := range steps {
		err := st(d)
		if err != nil {
			return err
		}
	}
	return nil
}

func initLogger(d *Dumper) error {
	conf := d.conf
	var (
		logger log.Logger
		err    error
		props  *pclog.ZapProperties
	)
	// conf.Logger != nil means dumpling is used as a library
	if conf.Logger != nil {
		logger = log.NewAppLogger(conf.Logger)
	} else {
		logger, props, err = log.InitAppLogger(&log.Config{
			Level:  conf.LogLevel,
			File:   conf.LogFile,
			Format: conf.LogFormat,
		})
		if err != nil {
			return errors.Trace(err)
		}
		pclog.ReplaceGlobals(logger.Logger, props)
		cli.LogLongVersion(logger)
	}
	d.tctx = d.tctx.WithLogger(logger)
	return nil
}

// createExternalStore is an initialization step of Dumper.
func createExternalStore(d *Dumper) error {
	tctx, conf := d.tctx, d.conf
	extStore, err := conf.createExternalStorage(tctx)
	if err != nil {
		return errors.Trace(err)
	}
	d.extStore = extStore
	return nil
}

// startHTTPService is an initialization step of Dumper.
func startHTTPService(d *Dumper) error {
	conf := d.conf
	if conf.StatusAddr != "" {
		go func() {
			err := startDumplingService(d.tctx, conf.StatusAddr)
			if err != nil {
				d.L().Warn("meet error when stopping dumpling http service", zap.Error(err))
			}
		}()
	}
	return nil
}

// openSQLDB is an initialization step of Dumper.
func openSQLDB(d *Dumper) error {
	conf := d.conf
	pool, err := sql.Open("mysql", conf.GetDSN(""))
	if err != nil {
		return errors.Trace(err)
	}
	d.dbHandle = pool
	return nil
}

// detectServerInfo is an initialization step of Dumper.
func detectServerInfo(d *Dumper) error {
	db, conf := d.dbHandle, d.conf
	versionStr, err := SelectVersion(db)
	if err != nil {
		conf.ServerInfo = ServerInfoUnknown
		return err
	}
	conf.ServerInfo = ParseServerInfo(d.tctx, versionStr)
	return nil
}

// resolveAutoConsistency is an initialization step of Dumper.
func resolveAutoConsistency(d *Dumper) error {
	conf := d.conf
	if conf.Consistency != "auto" {
		return nil
	}
	switch conf.ServerInfo.ServerType {
	case ServerTypeTiDB:
		conf.Consistency = "snapshot"
	case ServerTypeMySQL, ServerTypeMariaDB:
		conf.Consistency = "flush"
	default:
		conf.Consistency = "none"
	}
	return nil
}

// tidbSetPDClientForGC is an initialization step of Dumper.
func tidbSetPDClientForGC(d *Dumper) error {
	tctx, si, pool := d.tctx, d.conf.ServerInfo, d.dbHandle
	if si.ServerType != ServerTypeTiDB ||
		si.ServerVersion == nil ||
		si.ServerVersion.Compare(*gcSafePointVersion) < 0 {
		return nil
	}
	pdAddrs, err := GetPdAddrs(tctx, pool)
	if err != nil {
		return err
	}
	if len(pdAddrs) > 0 {
		doPdGC, err := checkSameCluster(tctx, pool, pdAddrs)
		if err != nil {
			tctx.L().Warn("meet error while check whether fetched pd addr and TiDB belong to one cluster", zap.Error(err), zap.Strings("pdAddrs", pdAddrs))
		} else if doPdGC {
			pdClient, err := pd.NewClientWithContext(tctx, pdAddrs, pd.SecurityOption{})
			if err != nil {
				tctx.L().Warn("create pd client to control GC failed", zap.Error(err), zap.Strings("pdAddrs", pdAddrs))
			}
			d.tidbPDClientForGC = pdClient
		}
	}
	return nil
}

// tidbGetSnapshot is an initialization step of Dumper.
func tidbGetSnapshot(d *Dumper) error {
	conf, doPdGC := d.conf, d.tidbPDClientForGC != nil
	consistency := conf.Consistency
	pool, tctx := d.dbHandle, d.tctx
	if conf.Snapshot == "" && (doPdGC || consistency == "snapshot") {
		conn, err := pool.Conn(tctx)
		if err != nil {
			tctx.L().Warn("cannot get snapshot from TiDB", zap.Error(err))
			return nil
		}
		snapshot, err := getSnapshot(conn)
		_ = conn.Close()
		if err != nil {
			tctx.L().Warn("cannot get snapshot from TiDB", zap.Error(err))
			return nil
		}
		conf.Snapshot = snapshot
		return nil
	}
	return nil
}

// tidbStartGCSavepointUpdateService is an initialization step of Dumper.
func tidbStartGCSavepointUpdateService(d *Dumper) error {
	tctx, pool, conf := d.tctx, d.dbHandle, d.conf
	snapshot, si := conf.Snapshot, conf.ServerInfo
	if d.tidbPDClientForGC != nil {
		snapshotTS, err := parseSnapshotToTSO(pool, snapshot)
		if err != nil {
			return err
		}
		go updateServiceSafePoint(tctx, d.tidbPDClientForGC, defaultDumpGCSafePointTTL, snapshotTS)
	} else if si.ServerType == ServerTypeTiDB {
		tctx.L().Warn("If the amount of data to dump is large, criteria: (data more than 60GB or dumped time more than 10 minutes)\n" +
			"you'd better adjust the tikv_gc_life_time to avoid export failure due to TiDB GC during the dump process.\n" +
			"Before dumping: run sql `update mysql.tidb set VARIABLE_VALUE = '720h' where VARIABLE_NAME = 'tikv_gc_life_time';` in tidb.\n" +
			"After dumping: run sql `update mysql.tidb set VARIABLE_VALUE = '10m' where VARIABLE_NAME = 'tikv_gc_life_time';` in tidb.\n")
	}
	return nil
}

func updateServiceSafePoint(tctx *tcontext.Context, pdClient pd.Client, ttl int64, snapshotTS uint64) {
	updateInterval := time.Duration(ttl/2) * time.Second
	tick := time.NewTicker(updateInterval)
	dumplingServiceSafePointID := fmt.Sprintf("%s_%d", dumplingServiceSafePointPrefix, time.Now().UnixNano())
	tctx.L().Info("generate dumpling gc safePoint id", zap.String("id", dumplingServiceSafePointID))

	for {
		tctx.L().Debug("update PD safePoint limit with ttl",
			zap.Uint64("safePoint", snapshotTS),
			zap.Int64("ttl", ttl))
		for retryCnt := 0; retryCnt <= 10; retryCnt++ {
			_, err := pdClient.UpdateServiceGCSafePoint(tctx, dumplingServiceSafePointID, ttl, snapshotTS)
			if err == nil {
				break
			}
			tctx.L().Debug("update PD safePoint failed", zap.Error(err), zap.Int("retryTime", retryCnt))
			select {
			case <-tctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
		select {
		case <-tctx.Done():
			return
		case <-tick.C:
		}
	}
}

// setSessionParam is an initialization step of Dumper.
func setSessionParam(d *Dumper) error {
	conf, pool := d.conf, d.dbHandle
	si := conf.ServerInfo
	consistency, snapshot := conf.Consistency, conf.Snapshot
	sessionParam := conf.SessionParams
	if si.ServerType == ServerTypeTiDB && conf.TiDBMemQuotaQuery != UnspecifiedSize {
		sessionParam[TiDBMemQuotaQueryName] = conf.TiDBMemQuotaQuery
	}
	var err error
	if snapshot != "" {
		if si.ServerType != ServerTypeTiDB {
			return errors.New("snapshot consistency is not supported for this server")
		}
		if consistency == consistencyTypeSnapshot {
			conf.ServerInfo.HasTiKV, err = CheckTiDBWithTiKV(pool)
			if err != nil {
				return err
			}
			if conf.ServerInfo.HasTiKV {
				sessionParam["tidb_snapshot"] = snapshot
			}
		}
	}
	if d.dbHandle, err = resetDBWithSessionParams(d.tctx, pool, conf.GetDSN(""), conf.SessionParams); err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (d *Dumper) renewSelectTableRegionFuncForLowerTiDB(tctx *tcontext.Context) error {
	conf := d.conf
	if !(conf.ServerInfo.ServerType == ServerTypeTiDB && conf.ServerInfo.ServerVersion != nil && conf.ServerInfo.HasTiKV &&
		conf.ServerInfo.ServerVersion.Compare(*decodeRegionVersion) >= 0 &&
		conf.ServerInfo.ServerVersion.Compare(*gcSafePointVersion) < 0) {
		tctx.L().Debug("no need to build region info because database is not TiDB 3.x")
		return nil
	}
	dbHandle, err := openDBFunc("mysql", conf.GetDSN(""))
	if err != nil {
		return errors.Trace(err)
	}
	defer dbHandle.Close()
	conn, err := dbHandle.Conn(tctx)
	if err != nil {
		return errors.Trace(err)
	}
	defer conn.Close()
	dbInfos, err := GetDBInfo(conn, DatabaseTablesToMap(conf.Tables))
	if err != nil {
		return errors.Trace(err)
	}
	regionsInfo, err := GetRegionInfos(conn)
	if err != nil {
		return errors.Trace(err)
	}
	tikvHelper := &helper.Helper{}
	tableInfos := tikvHelper.GetRegionsTableInfo(regionsInfo, dbInfos)

	tableInfoMap := make(map[string]map[string][]int64, len(conf.Tables))
	for _, region := range regionsInfo.Regions {
		tableList := tableInfos[region.ID]
		for _, table := range tableList {
			db, tbl := table.DB.Name.O, table.Table.Name.O
			if _, ok := tableInfoMap[db]; !ok {
				tableInfoMap[db] = make(map[string][]int64, len(conf.Tables[db]))
			}

			key, err := hex.DecodeString(region.StartKey)
			if err != nil {
				d.L().Debug("invalid region start key", zap.Error(err), zap.String("key", region.StartKey))
				continue
			}
			// Auto decode byte if needed.
			_, bs, err := codec.DecodeBytes(key, nil)
			if err == nil {
				key = bs
			}
			// Try to decode it as a record key.
			tableID, handle, err := tablecodec.DecodeRecordKey(key)
			if err != nil {
				d.L().Debug("fail to decode region start key", zap.Error(err), zap.String("key", region.StartKey), zap.Int64("tableID", tableID))
				continue
			}
			if handle.IsInt() {
				tableInfoMap[db][tbl] = append(tableInfoMap[db][tbl], handle.IntValue())
			} else {
				d.L().Debug("not an int handle", zap.Error(err), zap.Stringer("handle", handle))
			}
		}
	}
	for _, tbInfos := range tableInfoMap {
		for _, tbInfoLoop := range tbInfos {
			// make sure tbInfo is only used in this loop
			tbInfo := tbInfoLoop
			sort.Slice(tbInfo, func(i, j int) bool {
				return tbInfo[i] < tbInfo[j]
			})
		}
	}

	d.selectTiDBTableRegionFunc = func(tctx *tcontext.Context, conn *sql.Conn, dbName, tableName string) (pkFields []string, pkVals [][]string, err error) {
		pkFields, _, err = selectTiDBRowKeyFields(conn, dbName, tableName, checkTiDBTableRegionPkFields)
		if err != nil {
			return
		}
		if tbInfos, ok := tableInfoMap[dbName]; ok {
			if tbInfo, ok := tbInfos[tableName]; ok {
				pkVals = make([][]string, len(tbInfo))
				for i, val := range tbInfo {
					pkVals[i] = []string{strconv.FormatInt(val, 10)}
				}
			}
		}
		return
	}

	return nil
}
