package ovsdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/jinzhu/copier"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"k8s.io/klog/v2"

	"github.com/ibm/ovsdb-etcd/pkg/common"
	"github.com/ibm/ovsdb-etcd/pkg/libovsdb"
)

const ETCD_MAX_TXN_OPS = 128

const (
	/* ovsdb operations */
	E_DUP_UUIDNAME         = "duplicate uuid-name"
	E_CONSTRAINT_VIOLATION = "constraint violation"
	E_DOMAIN_ERROR         = "domain error"
	E_RANGE_ERROR          = "range error"
	E_TIMEOUT              = "timed out"
	E_NOT_SUPPORTED        = "not supported"
	E_ABORTED              = "aborted"
	E_NOT_OWNER            = "not owner"

	/* ovsdb transaction */
	E_INTEGRITY_VIOLATION = "referential integrity violation"
	E_RESOURCES_EXHAUSTED = "resources exhausted"
	E_IO_ERROR            = "I/O error"

	/* ovsdb extention */
	E_DUP_UUID         = "duplicate uuid"
	E_INTERNAL_ERROR   = "internal error"
	E_OVSDB_ERROR      = "ovsdb error"
	E_PERMISSION_ERROR = "permission error"
	E_SYNTAX_ERROR     = "syntax error or unknown column"
)

func isEqualSet(expected, actual interface{}) bool {
	expectedSet := expected.(libovsdb.OvsSet)
	actualSet := actual.(libovsdb.OvsSet)
	for _, expectedVal := range expectedSet.GoSet {
		foundVal := false
		for _, actualVal := range actualSet.GoSet {
			if isEqualValue(expectedVal, actualVal) {
				foundVal = true
			}
		}
		if !foundVal {
			return false
		}
	}
	return true
}

func isEqualMap(expected, actual interface{}) bool {
	expectedMap := expected.(libovsdb.OvsMap)
	actualMap := actual.(libovsdb.OvsMap)
	for key, expectedVal := range expectedMap.GoMap {
		actualVal, ok := actualMap.GoMap[key]
		if !ok {
			return false
		}
		if !isEqualValue(expectedVal, actualVal) {
			return false
		}
	}
	return true
}

func isEqualValue(expected, actual interface{}) bool {
	return reflect.DeepEqual(expected, actual)
}

func isEqualColumn(columnSchema *libovsdb.ColumnSchema, expected, actual interface{}) bool {
	switch columnSchema.Type {
	case libovsdb.TypeSet:
		return isEqualSet(expected, actual)
	case libovsdb.TypeMap:
		return isEqualMap(expected, actual)
	default:
		return isEqualValue(expected, actual)
	}
}

func isEqualRow(tableSchema *libovsdb.TableSchema, expectedRow, actualRow *map[string]interface{}) (bool, error) {
	for column, expected := range *expectedRow {
		columnSchema, err := tableSchema.LookupColumn(column)
		if err != nil {
			klog.Errorf("Schema doesn't contain column %s", column)
			return false, errors.New(E_CONSTRAINT_VIOLATION)
		}
		actual := (*actualRow)[column]
		if !isEqualColumn(columnSchema, expected, actual) {
			return false, nil
		}

	}
	return true, nil
}

// XXX: move to libovsdb
const (
	COL_UUID    = "_uuid"
	COL_VERSION = "_version"
)

// XXX: move to libovsdb
const (
	OP_INSERT  = "insert"
	OP_SELECT  = "select"
	OP_UPDATE  = "update"
	OP_MUTATE  = "mutate"
	OP_DELETE  = "delete"
	OP_WAIT    = "wait"
	OP_COMMIT  = "commit"
	OP_ABORT   = "abort"
	OP_COMMENT = "comment"
	OP_ASSERT  = "assert"
)

func etcdOpKey(op clientv3.Op) string {
	v := reflect.ValueOf(op)
	f := v.FieldByName("key")
	k := f.Bytes()
	return string(k)
}

func (txn *Transaction) etcdRemoveDupThen() {
	newThen := []*clientv3.Op{}
	for curr, op := range txn.etcd.Then {
		key := etcdOpKey(op)
		klog.V(6).Infof("adding key %s index %d", key, curr)
		newThen = append(newThen, &txn.etcd.Then[curr])
	}

	prevKeyIndex := map[string]int{}
	for curr, op := range newThen {
		key := etcdOpKey(*op)
		prev, ok := prevKeyIndex[key]
		if ok {
			klog.V(6).Infof("removing key %s index %d", key, prev)
			newThen[prev] = nil
		}
		prevKeyIndex[key] = curr
	}

	txn.etcd.Then = []clientv3.Op{}
	for _, op := range newThen {
		if op != nil {
			txn.etcd.Then = append(txn.etcd.Then, *op)
		}
	}
}

func etcdEventKey(ev *clientv3.Event) string {
	if ev.Kv != nil {
		return string(ev.Kv.Key)
	}
	if ev.PrevKv != nil {
		return string(ev.PrevKv.Key)
	}
	panic(fmt.Sprintf("can't extract key from %v", ev))
}

func (txn *Transaction) etcdRemoveDupEvents() {
	prevEvents := []*clientv3.Event{}
	newEvents := []*clientv3.Event{}
	for i, curr := range txn.etcd.Events {
		prevEvents = append(prevEvents, txn.etcd.Events[i])
		newEvents = append(newEvents, txn.etcd.Events[i])
		if curr == nil {
			txn.etcd.EventsNilCount++
			continue
		}
		key := etcdEventKey(curr)
		klog.V(6).Infof("adding key '%s' index %d", key, i)
	}

	prevKeyIndex := map[string]int{}
	for i, curr := range newEvents {
		if curr == nil {
			continue
		}
		key := etcdEventKey(curr)
		prevIndex, ok := prevKeyIndex[key]
		if ok {
			prev := prevEvents[prevIndex]
			if etcdEventIsModify(curr) && etcdEventIsCreate(prev) {
				newEvents[i] = etcdEventCreateFromModify(curr)
			}
			klog.V(6).Infof("removing key %s index %d", key, i)
			newEvents[i] = nil
		}
		prevKeyIndex[key] = i
	}

	txn.etcd.Events = []*clientv3.Event{}
	for _, curr := range newEvents {
		if curr == nil {
			continue
		}
		txn.etcd.Events = append(txn.etcd.Events, curr)
	}
}

func (txn *Transaction) etcdRemoveDup() {
	klog.V(6).Infof("etcd remove dups: %s", txn.etcd)
	txn.etcdRemoveDupThen()
	txn.etcdRemoveDupEvents()
	txn.etcd.Assert()
}

func (txn *Transaction) etcdTranaction() (*clientv3.TxnResponse, error) {
	klog.V(6).Infof("etcd transaction: %s", txn.etcd)

	// etcds := txn.etcd.Split() // split
	etcds := []*Etcd{txn.etcd} // don't split

	for i, child := range etcds {
		klog.V(6).Infof("etcd processing(%d): %s", i, child)
		err := child.Commit()
		if err != nil {
			klog.V(6).Infof("etcd processing(%d): %s", i, err)
			return nil, errors.New(E_IO_ERROR)
		}
		txn.cache.GetFromEtcd(child.Res)
	}

	err := txn.cache.Unmarshal(txn.schemas)
	if err != nil {
		return nil, err
	}

	err = txn.cache.Validate(txn.schemas)
	if err != nil {
		return nil, err
	}

	return txn.etcd.Res, nil
}

// XXX: move to db
type KeyValue struct {
	Key   common.Key
	Value map[string]interface{}
}

// XXX: move to db
func NewKeyValue(etcdKV *mvccpb.KeyValue) (*KeyValue, error) {
	kv := new(KeyValue)

	/* key */
	key, err := common.ParseKey(string(etcdKV.Key))
	if err != nil {
		return nil, err
	}
	kv.Key = *key
	/* value */
	err = json.Unmarshal(etcdKV.Value, &kv.Value)
	if err != nil {
		return nil, err
	}

	return kv, nil
}

func (kv *KeyValue) Dump() {
	fmt.Printf("%s --> %v\n", kv.Key, kv.Value)
}

type Cache map[string]DatabaseCache
type DatabaseCache map[string]TableCache
type TableCache map[string]*map[string]interface{}

func (c *Cache) Database(dbname string) DatabaseCache {
	db, ok := (*c)[dbname]
	if !ok {
		db = DatabaseCache{}
		(*c)[dbname] = db
	}
	return db
}

func (c *Cache) Table(dbname, table string) TableCache {
	db := c.Database(dbname)
	tb, ok := db[table]
	if !ok {
		tb = TableCache{}
		db[table] = tb
	}
	return tb
}

func (c *Cache) Row(key common.Key) *map[string]interface{} {
	tb := c.Table(key.DBName, key.TableName)
	_, ok := tb[key.UUID]
	if !ok {
		tb[key.UUID] = new(map[string]interface{})
	}
	return tb[key.UUID]
}

func (c *Cache) GetFromEtcdKV(kvs []*mvccpb.KeyValue) error {
	for _, x := range kvs {
		kv, err := NewKeyValue(x)
		if err != nil {
			return err
		}
		row := c.Row(kv.Key)
		(*row) = kv.Value
	}
	return nil
}

func (cache *Cache) GetFromEtcd(res *clientv3.TxnResponse) {
	for _, r := range res.Responses {
		switch v := r.Response.(type) {
		case *etcdserverpb.ResponseOp_ResponseRange:
			cache.GetFromEtcdKV(v.ResponseRange.Kvs)
		}
	}
}

func (cache *Cache) Unmarshal(schemas libovsdb.Schemas) error {
	for database, databaseCache := range *cache {
		for table, tableCache := range databaseCache {
			for _, row := range tableCache {
				err := schemas.Unmarshal(database, table, row)
				if err != nil {
					klog.Errorf("%s", err)
					return errors.New(E_INTEGRITY_VIOLATION)
				}
			}
		}
	}
	return nil
}

func (cache *Cache) Validate(schemas libovsdb.Schemas) error {
	for database, databaseCache := range *cache {
		for table, tableCache := range databaseCache {
			for _, row := range tableCache {
				err := schemas.Validate(database, table, row)
				if err != nil {
					klog.Errorf("%s", err)
					return errors.New(E_INTEGRITY_VIOLATION)
				}
			}
		}
	}
	return nil
}

type MapUUID map[string]string

func (mapUUID MapUUID) Set(uuidName, uuid string) {
	klog.V(6).Infof("setting named-uuid %s to uuid %s", uuidName, uuid)
	mapUUID[uuidName] = uuid
}

func (mapUUID MapUUID) Get(uuidName string) (string, error) {
	uuid, ok := mapUUID[uuidName]
	if !ok {
		klog.Errorf("Can't get named-uuid %s", uuidName)
		return "", errors.New(E_CONSTRAINT_VIOLATION)
	}
	return uuid, nil
}

func (mapUUID MapUUID) ResolvUUID(value interface{}) (interface{}, error) {
	namedUuid, _ := value.(libovsdb.UUID)
	if namedUuid.GoUUID != "" && namedUuid.ValidateUUID() != nil {
		uuid, err := mapUUID.Get(namedUuid.GoUUID)
		if err != nil {
			return nil, err
		}
		value = libovsdb.UUID{GoUUID: uuid}
	}
	return value, nil
}

func (mapUUID MapUUID) ResolvSet(value interface{}) (interface{}, error) {
	oldset, _ := value.(libovsdb.OvsSet)
	newset := libovsdb.OvsSet{}
	for _, oldval := range oldset.GoSet {
		newval, err := mapUUID.ResolvUUID(oldval)
		if err != nil {
			return nil, err
		}
		newset.GoSet = append(newset.GoSet, newval)
	}
	return newset, nil
}

func (mapUUID MapUUID) ResolvMap(value interface{}) (interface{}, error) {
	oldmap, _ := value.(libovsdb.OvsMap)
	newmap := libovsdb.OvsMap{GoMap: map[interface{}]interface{}{}}
	for key, oldval := range oldmap.GoMap {
		newval, err := mapUUID.ResolvUUID(oldval)
		if err != nil {
			return nil, err
		}
		newmap.GoMap[key] = newval
	}
	return newmap, nil
}

func (mapUUID MapUUID) Resolv(value interface{}) (interface{}, error) {
	switch value.(type) {
	case libovsdb.UUID:
		return mapUUID.ResolvUUID(value)
	case libovsdb.OvsSet:
		return mapUUID.ResolvSet(value)
	case libovsdb.OvsMap:
		return mapUUID.ResolvMap(value)
	default:
		return value, nil
	}
}

func (mapUUID MapUUID) ResolvRow(row *map[string]interface{}) error {
	for column, value := range *row {
		value, err := mapUUID.Resolv(value)
		if err != nil {
			return err
		}
		(*row)[column] = value
	}
	return nil
}

type Etcd struct {
	Cli            *clientv3.Client
	Ctx            context.Context
	If             []clientv3.Cmp
	Then           []clientv3.Op
	Else           []clientv3.Op
	Res            *clientv3.TxnResponse
	EventsNilCount int
	Events         []*clientv3.Event
}

func (etcd *Etcd) Assert() {
	if len(etcd.Then) != (len(etcd.Events) + etcd.EventsNilCount) {
		panic(fmt.Sprintf("etcd: #then != #events: %s", etcd))
	}
}

func (etcd *Etcd) EventsDump() string {
	printable := []clientv3.Event{}
	for _, ev := range etcd.Events {
		printable = append(printable, *ev)
	}
	return fmt.Sprintf("%v", printable)
}

func NewEtcd(parent *Etcd) *Etcd {
	return &Etcd{
		Ctx: parent.Ctx,
		Cli: parent.Cli,
	}
}
func (etcd *Etcd) Clear() {
	etcd.If = []clientv3.Cmp{}
	etcd.Then = []clientv3.Op{}
	etcd.Else = []clientv3.Op{}
	etcd.Res = nil
	etcd.EventsNilCount = 0
	etcd.Events = []*clientv3.Event{}
	etcd.Assert()
}

func (etcd Etcd) String() string {
	return fmt.Sprintf("#then %d, #events %d, #events-nil %d", len(etcd.Then), len(etcd.Events), etcd.EventsNilCount)
}

func (etcd *Etcd) Commit() error {
	res, err := etcd.Cli.Txn(etcd.Ctx).If(etcd.If...).Then(etcd.Then...).Else(etcd.Else...).Commit()
	if err != nil {
		return err
	}
	etcd.Res = res
	return nil
}

func (etcd *Etcd) Split() []*Etcd {
	split := []*Etcd{}
	child := NewEtcd(etcd)
	split = append(split, child)
	for _, op := range etcd.Then {
		child.Then = append(child.Then, op)
		if len(child.Then) == ETCD_MAX_TXN_OPS {
			child = NewEtcd(etcd)
			split = append(split, child)
		}
	}
	return split
}

type TxnLock struct {
	root      sync.Mutex
	databases map[string]*sync.Mutex
}

func NewTxnLock() *TxnLock {
	return &TxnLock{
		databases: map[string]*sync.Mutex{},
	}
}

func (lock *TxnLock) Lock(dbname string) {
	lock.root.Lock()
	defer lock.root.Unlock()
	db, ok := lock.databases[dbname]
	if !ok {
		lock.databases[dbname] = new(sync.Mutex)
	}
	db, ok = lock.databases[dbname]
	if !ok {
		panic(fmt.Sprintf("missing transaction lock for database %s", dbname))
	}
	db.Lock()
}

func (lock *TxnLock) Unlock(dbname string) {
	lock.root.Lock()
	defer lock.root.Unlock()
	db, ok := lock.databases[dbname]
	if !ok {
		panic(fmt.Sprintf("missing transaction lock for database %s", dbname))
	}
	defer db.Unlock()
}

type Transaction struct {
	/* lock */
	lock *TxnLock

	/* ovs */
	schemas  libovsdb.Schemas
	request  libovsdb.Transact
	response libovsdb.TransactResponse

	/* cache */
	cache   Cache
	mapUUID MapUUID

	/* etcd */
	etcd *Etcd
}

func NewTransaction(cli *clientv3.Client, request *libovsdb.Transact) *Transaction {
	klog.V(6).Infof("new transaction [with size %d]: %s", len(request.Operations), request)
	txn := new(Transaction)
	txn.lock = NewTxnLock()
	txn.cache = Cache{}
	txn.mapUUID = MapUUID{}
	txn.schemas = libovsdb.Schemas{}
	txn.request = *request
	txn.response.Result = make([]libovsdb.OperationResult, len(request.Operations))
	txn.etcd = new(Etcd)
	txn.etcd.Ctx = context.TODO()
	txn.etcd.Cli = cli
	return txn
}

type ovsOpCallback func(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error

var ovsOpCallbackMap = map[string][2]ovsOpCallback{
	OP_INSERT:  {preInsert, doInsert},
	OP_SELECT:  {preSelect, doSelect},
	OP_UPDATE:  {preUpdate, doUpdate},
	OP_MUTATE:  {preMutate, doMutate},
	OP_DELETE:  {preDelete, doDelete},
	OP_WAIT:    {preWait, doWait},
	OP_COMMIT:  {preCommit, doCommit},
	OP_ABORT:   {preAbort, doAbort},
	OP_COMMENT: {preComment, doComment},
	OP_ASSERT:  {preAssert, doAssert},
}

func (txn *Transaction) AddSchemaFromFile(path string) error {
	return txn.schemas.AddFromFile(path)
}

func (txn *Transaction) AddSchema(databaseSchema *libovsdb.DatabaseSchema) {
	txn.schemas.Add(databaseSchema)
}

func (txn *Transaction) Commit() (int64, error) {
	txn.lock.Lock(txn.request.DBName)
	defer txn.lock.Unlock(txn.request.DBName)

	var err error

	/* verify that select is not intermixed with other operations */
	hasSelect := false
	hasOther := false
	for _, ovsOp := range txn.request.Operations {
		if ovsOp.Op == OP_SELECT {
			hasSelect = true
		} else {
			hasOther = true
		}
	}
	if hasSelect && hasOther {
		klog.Errorf("Can't mix select with other operations")
		err := errors.New(E_CONSTRAINT_VIOLATION)
		errStr := err.Error()
		txn.response.Error = &errStr
		return -1, err
	}

	/* fetch needed data from database needed to perform the operation */
	txn.etcd.Clear()
	for i, ovsOp := range txn.request.Operations {
		err := ovsOpCallbackMap[ovsOp.Op][0](txn, &ovsOp, &txn.response.Result[i])
		if err != nil {
			errStr := err.Error()
			txn.response.Result[i].SetError(errStr)
			txn.response.Error = &errStr
			return -1, err
		}

		if err = txn.cache.Validate(txn.schemas); err != nil {
			panic(fmt.Sprintf("validation of %s failed: %s", ovsOp, err.Error()))
		}
	}
	_, err = txn.etcdTranaction()
	if err != nil {
		errStr := err.Error()
		txn.response.Error = &errStr
		return -1, err
	}

	/* commit actual transactional changes to database */
	txn.etcd.Clear()
	for i, ovsOp := range txn.request.Operations {
		err = ovsOpCallbackMap[ovsOp.Op][1](txn, &ovsOp, &txn.response.Result[i])
		if err != nil {
			errStr := err.Error()
			txn.response.Result[i].SetError(errStr)
			txn.response.Error = &errStr
			return -1, err
		}

		if err = txn.cache.Validate(txn.schemas); err != nil {
			panic(fmt.Sprintf("validation of %s failed: %s", ovsOp, err.Error()))
		}
	}

	txn.etcdRemoveDup()
	trResponse, err := txn.etcdTranaction()
	if err != nil {
		errStr := err.Error()
		txn.response.Error = &errStr
		return -1, err
	}

	return trResponse.Header.Revision, nil
}

// XXX: move to db
func makeValue(row *map[string]interface{}) (string, error) {
	b, err := json.Marshal(*row)
	if err != nil {
		klog.Errorf("Failed json marshal: %s", err.Error())
		return "", err
	}
	return string(b), nil
}

// TODO: we should not add uuid to etcd
func setRowUUID(row *map[string]interface{}, uuid string) {
	(*row)[COL_UUID] = libovsdb.UUID{GoUUID: uuid}
}

const (
	FN_LT = "<"
	FN_LE = "<="
	FN_EQ = "=="
	FN_NE = "!="
	FN_GE = ">="
	FN_GT = ">"
	FN_IN = "includes"
	FN_EX = "excludes"
)

type Condition struct {
	Column       string
	Function     string
	Value        interface{}
	ColumnSchema *libovsdb.ColumnSchema
}

func NewCondition(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, condition []interface{}) (*Condition, error) {
	if len(condition) != 3 {
		klog.Errorf("Expected 3 elements in condition: %v", condition)
		return nil, errors.New(E_INTERNAL_ERROR)
	}

	column, ok := condition[0].(string)
	if !ok {
		klog.Errorf("Failed to convert column to string: %v", condition)
		return nil, errors.New(E_INTERNAL_ERROR)
	}

	var columnSchema *libovsdb.ColumnSchema
	var err error
	if column != COL_UUID && column != COL_VERSION {
		columnSchema, err = tableSchema.LookupColumn(column)
		if err != nil {
			return nil, errors.New(E_CONSTRAINT_VIOLATION)
		}
	}

	fn, ok := condition[1].(string)
	if !ok {
		klog.Errorf("Failed to convert function to string: %v", condition)
		return nil, errors.New(E_INTERNAL_ERROR)
	}

	value := condition[2]
	if columnSchema != nil {
		tmp, err := columnSchema.Unmarshal(value)
		if err != nil {
			klog.Errorf("Failed to unmarsahl condition (columne %s, type %s, value %s)", column, columnSchema.Type, value)
			return nil, errors.New(E_INTERNAL_ERROR)
		}
		value = tmp
	} else if column == COL_UUID {
		tmp, err := libovsdb.UnmarshalUUID(value)
		if err != nil {
			klog.Errorf("Failed to unamrshal condition (columne %s, type %s, value %s)", column, "uuid", value)
			return nil, errors.New(E_INTERNAL_ERROR)
		}
		value = tmp
	}

	tmp, err := mapUUID.Resolv(value)
	if err != nil {
		klog.Errorf("Failed to resolve named-uuid condition (column %s, value %s)", column, value)
		return nil, errors.New(E_INTERNAL_ERROR)
	}
	value = tmp

	return &Condition{
		Column:       column,
		Function:     fn,
		Value:        value,
		ColumnSchema: columnSchema,
	}, nil
}

func (c *Condition) CompareInteger(row *map[string]interface{}) (bool, error) {
	actual, ok := (*row)[c.Column].(int)
	if !ok {
		klog.Errorf("Failed to convert row value: %v", (*row)[c.Column])
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	fn := c.Function
	expected, ok := c.Value.(int)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	if (fn == FN_EQ || fn == FN_IN) && actual == expected {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && actual != expected {
		return true, nil
	}
	if fn == FN_GT && actual > expected {
		return true, nil
	}
	if fn == FN_GE && actual >= expected {
		return true, nil
	}
	if fn == FN_LT && actual < expected {
		return true, nil
	}
	if fn == FN_LE && actual <= expected {
		return true, nil
	}
	return false, nil
}

func (c *Condition) CompareReal(row *map[string]interface{}) (bool, error) {
	actual, ok := (*row)[c.Column].(float64)
	if !ok {
		klog.Errorf("Failed to convert row value: %v", (*row)[c.Column])
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	fn := c.Function
	expected, ok := c.Value.(float64)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	if (fn == FN_EQ || fn == FN_IN) && actual == expected {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && actual != expected {
		return true, nil
	}
	if fn == FN_GT && actual > expected {
		return true, nil
	}
	if fn == FN_GE && actual >= expected {
		return true, nil
	}
	if fn == FN_LT && actual < expected {
		return true, nil
	}
	if fn == FN_LE && actual <= expected {
		return true, nil
	}
	return false, nil
}

func (c *Condition) CompareBoolean(row *map[string]interface{}) (bool, error) {
	actual, ok := (*row)[c.Column].(bool)
	if !ok {
		klog.Errorf("Failed to convert row value: %v", (*row)[c.Column])
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	fn := c.Function
	expected, ok := c.Value.(bool)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	if (fn == FN_EQ || fn == FN_IN) && actual == expected {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && actual != expected {
		return true, nil
	}
	return false, nil
}

func (c *Condition) CompareString(row *map[string]interface{}) (bool, error) {
	actual, ok := (*row)[c.Column].(string)
	if !ok {
		klog.Errorf("Failed to convert row value: %v", (*row)[c.Column])
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	fn := c.Function
	expected, ok := c.Value.(string)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	if (fn == FN_EQ || fn == FN_IN) && actual == expected {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && actual != expected {
		return true, nil
	}
	return false, nil
}

func (c *Condition) CompareUUID(row *map[string]interface{}) (bool, error) {
	var actual libovsdb.UUID
	ar, ok := (*row)[c.Column].([]interface{})
	if ok {
		actual = libovsdb.UUID{GoUUID: ar[1].(string)}
	} else {
		actual, ok = (*row)[c.Column].(libovsdb.UUID)
		if !ok {
			klog.Errorf("Failed to convert row value: %T %+v", (*row)[c.Column], (*row)[c.Column])
			return false, errors.New(E_CONSTRAINT_VIOLATION)
		}
	}
	fn := c.Function
	expected, ok := c.Value.(libovsdb.UUID)
	if !ok {
		klog.Errorf("Failed to convert condition value: %+v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	if (fn == FN_EQ || fn == FN_IN) && actual.GoUUID == expected.GoUUID {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && actual.GoUUID != expected.GoUUID {
		return true, nil
	}
	return false, nil
}

func (c *Condition) CompareEnum(row *map[string]interface{}) (bool, error) {
	switch c.ColumnSchema.TypeObj.Key.Type {
	case libovsdb.TypeString:
		return c.CompareString(row)
	default:
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
}

func (c *Condition) CompareSet(row *map[string]interface{}) (bool, error) {
	actual, ok := (*row)[c.Column].(libovsdb.OvsSet)
	if !ok {
		klog.Errorf("Failed to convert row value: %v", (*row)[c.Column])
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	fn := c.Function
	expected, ok := c.Value.(libovsdb.OvsSet)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	if (fn == FN_EQ || fn == FN_IN) && isEqualSet(actual, expected) {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && !isEqualSet(actual, expected) {
		return true, nil
	}
	return false, nil
}

func (c *Condition) CompareMap(row *map[string]interface{}) (bool, error) {
	actual, ok := (*row)[c.Column].(libovsdb.OvsMap)
	if !ok {
		klog.Errorf("Failed to convert row value: %v", (*row)[c.Column])
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
	fn := c.Function
	expected, ok := c.Value.(libovsdb.OvsMap)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", c.Value)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	if (fn == FN_EQ || fn == FN_IN) && isEqualMap(actual, expected) {
		return true, nil
	}
	if (fn == FN_NE || fn == FN_EX) && !isEqualMap(actual, expected) {
		return true, nil
	}
	return false, nil
}

func (c *Condition) Compare(row *map[string]interface{}) (bool, error) {
	switch c.Column {
	case COL_UUID:
		return c.CompareUUID(row)
	case COL_VERSION:
		klog.Errorf("Unsupported field comparison: %s", COL_VERSION)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}

	switch c.ColumnSchema.Type {
	case libovsdb.TypeInteger:
		return c.CompareInteger(row)
	case libovsdb.TypeReal:
		return c.CompareReal(row)
	case libovsdb.TypeBoolean:
		return c.CompareBoolean(row)
	case libovsdb.TypeString:
		return c.CompareString(row)
	case libovsdb.TypeUUID:
		return c.CompareUUID(row)
	case libovsdb.TypeEnum:
		return c.CompareEnum(row)
	case libovsdb.TypeSet:
		return c.CompareSet(row)
	case libovsdb.TypeMap:
		return c.CompareMap(row)
	default:
		klog.Errorf("Usupported type comparison: %s", c.ColumnSchema.Type)
		return false, errors.New(E_CONSTRAINT_VIOLATION)
	}
}

func getUUIDIfExists(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, cond1 interface{}) (string, error) {
	cond2, ok := cond1.([]interface{})
	if !ok {
		klog.Errorf("Failed to convert row value: %v", cond1)
		return "", errors.New(E_INTERNAL_ERROR)
	}
	condition, err := NewCondition(tableSchema, mapUUID, cond2)
	if err != nil {
		return "", err
	}
	if condition.Column != COL_UUID {
		return "", nil
	}
	if condition.Function != FN_EQ && condition.Function != FN_IN {
		return "", nil
	}
	ovsUUID, ok := condition.Value.(libovsdb.UUID)
	if !ok {
		klog.Errorf("Failed to convert condition value: %v", condition.Value)
		return "", errors.New(E_INTERNAL_ERROR)
	}
	err = ovsUUID.ValidateUUID()
	if err != nil {
		klog.Errorf("Failed uuid validation: %s", err.Error())
		return "", err
	}
	return ovsUUID.GoUUID, err
}

func doesWhereContainCondTypeUUID(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, where *[]interface{}) (string, error) {
	if where == nil {
		return "", nil
	}
	for _, c := range *where {
		cond, ok := c.([]interface{})
		if !ok {
			klog.Errorf("Failed to convert row value: %v", c)
			return "", errors.New(E_INTERNAL_ERROR)
		}
		uuid, err := getUUIDIfExists(tableSchema, mapUUID, cond)
		if err != nil {
			return "", err
		}
		if uuid != "" {
			return uuid, nil
		}
	}
	return "", nil

}

func isRowSelectedByWhere(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, row *map[string]interface{}, where *[]interface{}) (bool, error) {
	if where == nil {
		return true, nil
	}
	for _, c := range *where {
		cond, ok := c.([]interface{})
		if !ok {
			klog.Errorf("Failed to convert condition value: %+v", c)
			return false, errors.New(E_INTERNAL_ERROR)
		}
		ok, err := isRowSelectedByCond(tableSchema, mapUUID, row, cond)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

func isRowSelectedByCond(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, row *map[string]interface{}, cond []interface{}) (bool, error) {
	condition, err := NewCondition(tableSchema, mapUUID, cond)
	if err != nil {
		return false, err
	}
	return condition.Compare(row)
}

// XXX: shared with monitors
func reduceRowByColumns(row *map[string]interface{}, columns *[]string) (*map[string]interface{}, error) {
	if columns == nil {
		return row, nil
	}
	newRow := map[string]interface{}{}
	for _, column := range *columns {
		newRow[column] = (*row)[column]
	}
	return &newRow, nil
}

const (
	MT_SUM        = "+="
	MT_DIFFERENCE = "-="
	MT_PRODUCT    = "*="
	MT_QUOTIENT   = "/="
	MT_REMAINDER  = "%="
	MT_INSERT     = "insert"
	MT_DELETE     = "delete"
)

type Mutation struct {
	Column       string
	Mutator      string
	Value        interface{}
	ColumnSchema *libovsdb.ColumnSchema
}

func NewMutation(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, mutation []interface{}) (*Mutation, error) {
	if len(mutation) != 3 {
		klog.Errorf("Expected 3 items in mutation object: %v", mutation)
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	column, ok := mutation[0].(string)
	if !ok {
		klog.Errorf("Can't convert mutation column: %v", mutation[0])
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	columnSchema, err := tableSchema.LookupColumn(column)
	if err != nil {
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	mt, ok := mutation[1].(string)
	if !ok {
		klog.Errorf("Can't convert mutation mutator: %v", mutation[1])
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	value := mutation[2]

	value, err = columnSchema.Unmarshal(value)
	if err != nil {
		klog.Errorf("failed unmarshal of column %s: %s", column, err.Error())
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	value, err = mapUUID.Resolv(value)
	if err != nil {
		klog.Errorf("failed resolv-namedUUID of column %s: %s", column, err.Error())
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	err = columnSchema.Validate(value)
	if err != nil {
		klog.Errorf("failed validate of column %s: %s", column, err.Error())
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}

	return &Mutation{
		Column:       column,
		Mutator:      mt,
		Value:        value,
		ColumnSchema: columnSchema,
	}, nil
}

func (m *Mutation) MutateInteger(row *map[string]interface{}) error {
	original := (*row)[m.Column].(int)
	value, ok := m.Value.(int)
	if !ok {
		klog.Errorf("Can't convert mutation value: %v", m.Value)
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	mutated := original
	var err error
	switch m.Mutator {
	case MT_SUM:
		mutated += value
	case MT_DIFFERENCE:
		mutated -= value
	case MT_PRODUCT:
		mutated *= value
	case MT_QUOTIENT:
		if value != 0 {
			mutated /= value
		} else {
			klog.Errorf("Can't devide by 0")
			err = errors.New(E_DOMAIN_ERROR)
		}
	case MT_REMAINDER:
		if value != 0 {
			mutated %= value
		} else {
			klog.Errorf("Can't modulo by 0")
			err = errors.New(E_DOMAIN_ERROR)
		}
	default:
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	(*row)[m.Column] = mutated
	return err
}

func (m *Mutation) MutateReal(row *map[string]interface{}) error {
	original := (*row)[m.Column].(float64)
	value, ok := m.Value.(float64)
	if !ok {
		klog.Errorf("Failed to convert mutation value: %v", m.Value)
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	mutated := original
	var err error
	switch m.Mutator {
	case MT_SUM:
		mutated += value
	case MT_DIFFERENCE:
		mutated -= value
	case MT_PRODUCT:
		mutated *= value
	case MT_QUOTIENT:
		if value != 0 {
			mutated /= value
		} else {
			klog.Errorf("Can't devide by 0")
			err = errors.New(E_DOMAIN_ERROR)
		}
	default:
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	(*row)[m.Column] = mutated
	return err
}

func inSet(set *libovsdb.OvsSet, a interface{}) bool {
	for _, b := range set.GoSet {
		if isEqualValue(a, b) {
			return true
		}
	}
	return false
}

func insertToSet(original *libovsdb.OvsSet, toInsert interface{}) (*libovsdb.OvsSet, error) {
	toInsertSet, ok := toInsert.(libovsdb.OvsSet)
	if !ok {
		klog.Errorf("Failed to convert mutation value: %v", toInsert)
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}
	mutated := new(libovsdb.OvsSet)
	copier.Copy(mutated, original)
	for _, v := range toInsertSet.GoSet {
		if !inSet(original, v) {
			mutated.GoSet = append(mutated.GoSet, v)
		}
	}
	return mutated, nil
}

func deleteFromSet(original *libovsdb.OvsSet, toDelete interface{}) (*libovsdb.OvsSet, error) {
	toDeleteSet, ok := toDelete.(libovsdb.OvsSet)
	if !ok {
		klog.Errorf("Failed to convert mutation value: %v", toDelete)
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}
	mutated := new(libovsdb.OvsSet)
	for _, current := range original.GoSet {
		found := false
		for _, v := range toDeleteSet.GoSet {
			if isEqualValue(current, v) {
				found = true
				break
			}
		}
		if !found {
			mutated.GoSet = append(mutated.GoSet, current)
		}
	}
	return mutated, nil
}

func (m *Mutation) MutateSet(row *map[string]interface{}) error {
	original := (*row)[m.Column].(libovsdb.OvsSet)
	var mutated *libovsdb.OvsSet
	var err error
	switch m.Mutator {
	case MT_INSERT:
		mutated, err = insertToSet(&original, m.Value)
	case MT_DELETE:
		mutated, err = deleteFromSet(&original, m.Value)
	default:
		klog.Errorf("Unsupported mutation mutator: %s", m.Mutator)
		err = errors.New(E_CONSTRAINT_VIOLATION)
	}
	if err != nil {
		return err
	}
	(*row)[m.Column] = *mutated
	return nil
}

func insertToMap(original *libovsdb.OvsMap, toInsert interface{}) (*libovsdb.OvsMap, error) {
	mutated := new(libovsdb.OvsMap)
	copier.Copy(&mutated, &original)
	switch toInsert := toInsert.(type) {
	case libovsdb.OvsMap:
		for k, v := range toInsert.GoMap {
			mutated.GoMap[k] = v
		}
	default:
		klog.Errorf("Unsupported mutator value type: %+v", toInsert)
		return nil, errors.New(E_CONSTRAINT_VIOLATION)
	}
	return mutated, nil
}

func deleteFromMap(original *libovsdb.OvsMap, toDelete interface{}) (*libovsdb.OvsMap, error) {
	mutated := new(libovsdb.OvsMap)
	copier.Copy(&mutated, &original)
	switch toDelete := toDelete.(type) {
	case libovsdb.OvsMap:
		for k, v := range toDelete.GoMap {
			if mutated.GoMap[k] == v {
				delete(mutated.GoMap, k)
			}
		}
	case libovsdb.OvsSet:
		for _, k := range toDelete.GoSet {
			delete(mutated.GoMap, k)
		}
	}
	return mutated, nil
}

func (m *Mutation) MutateMap(row *map[string]interface{}) error {
	original := (*row)[m.Column].(libovsdb.OvsMap)
	mutated := new(libovsdb.OvsMap)
	var err error
	switch m.Mutator {
	case MT_INSERT:
		mutated, err = insertToMap(&original, m.Value)
	case MT_DELETE:
		mutated, err = deleteFromMap(&original, m.Value)
	default:
		klog.Errorf("Unsupported mutation mutator: %s", m.Mutator)
		err = errors.New(E_CONSTRAINT_VIOLATION)
	}
	if err != nil {
		return err
	}
	(*row)[m.Column] = *mutated
	return nil
}

func (m *Mutation) Mutate(row *map[string]interface{}) error {
	switch m.Column {
	case COL_UUID, COL_VERSION:
		klog.Errorf("Can't mutate column: %s", m.Column)
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	if m.ColumnSchema.Mutable != nil && !*m.ColumnSchema.Mutable {
		klog.Errorf("Can't mutate unmutable column: %s", m.Column)
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	switch m.ColumnSchema.Type {
	case libovsdb.TypeInteger:
		return m.MutateInteger(row)
	case libovsdb.TypeReal:
		return m.MutateReal(row)
	case libovsdb.TypeSet:
		return m.MutateSet(row)
	case libovsdb.TypeMap:
		return m.MutateMap(row)
	default:
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
}

func RowMutate(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, original *map[string]interface{}, mutations *[]interface{}) error {
	mutated := &map[string]interface{}{}
	copier.Copy(mutated, original)
	for _, mt := range *mutations {
		mutation, err := NewMutation(tableSchema, mapUUID, mt.([]interface{}))
		if err != nil {
			return err
		}
		err = mutation.Mutate(mutated)
		if err != nil {
			return err
		}
	}
	copier.Copy(original, mutated)
	return nil
}

func RowUpdate(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, original *map[string]interface{}, update *map[string]interface{}) error {
	for column, value := range *update {
		columnSchema, err := tableSchema.LookupColumn(column)
		if err != nil {
			return errors.New(E_CONSTRAINT_VIOLATION)
		}
		switch column {
		case COL_UUID, COL_VERSION:
			klog.Errorf("failed update of column: %s", column)
			return errors.New(E_CONSTRAINT_VIOLATION)
		}
		if columnSchema.Mutable != nil && !*columnSchema.Mutable {
			klog.Errorf("failed update of unmutable column: %s", column)
			return errors.New(E_CONSTRAINT_VIOLATION)
		}

		(*original)[column] = value
	}
	return nil
}

func etcdGetData(txn *Transaction, key *common.Key) {
	etcdOp := clientv3.OpGet(key.String(), clientv3.WithPrefix())
	// XXX: eliminate duplicate GETs
	txn.etcd.Then = append(txn.etcd.Then, etcdOp)
}

func etcdGetByWhere(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		return errors.New(E_INTERNAL_ERROR)
	}
	uuid, err := doesWhereContainCondTypeUUID(tableSchema, txn.mapUUID, ovsOp.Where)
	if err != nil {
		return err
	}
	key := common.NewDataKey(txn.request.DBName, *ovsOp.Table, uuid)
	etcdGetData(txn, &key)
	return nil
}

func etcdEventIsCreate(ev *clientv3.Event) bool {
	if ev.Type != mvccpb.PUT {
		return false
	}
	return ev.Kv.CreateRevision == ev.Kv.ModRevision
}

func etcdEventIsModify(ev *clientv3.Event) bool {
	if ev.Type != mvccpb.PUT {
		return false
	}
	return ev.Kv.CreateRevision < ev.Kv.ModRevision
}

func etcdEventCreateFromModify(ev *clientv3.Event) *clientv3.Event {
	key := string(ev.Kv.Key)
	val := string(ev.Kv.Value)
	return etcdEventCreate(key, val)
}

func etcdEventCreate(key, val string) *clientv3.Event {
	return &clientv3.Event{
		Type: mvccpb.PUT,
		Kv: &mvccpb.KeyValue{
			Key:            []byte(key),
			Value:          []byte(val),
			CreateRevision: 1,
			ModRevision:    1,
		},
	}

}

func etcdEventModify(key, val, prevVal string) *clientv3.Event {
	return &clientv3.Event{
		Type: mvccpb.PUT,
		Kv: &mvccpb.KeyValue{
			Key:            []byte(key),
			Value:          []byte(val),
			CreateRevision: 1,
			ModRevision:    2,
		},
		PrevKv: &mvccpb.KeyValue{
			Key:            []byte(key),
			Value:          []byte(prevVal),
			CreateRevision: 1,
			ModRevision:    1,
		},
	}
}

func etcdEventDelete(key, prevVal string) *clientv3.Event {
	return &clientv3.Event{
		Type: mvccpb.DELETE,
		PrevKv: &mvccpb.KeyValue{
			Key:            []byte(key),
			Value:          []byte(prevVal),
			CreateRevision: 1,
			ModRevision:    1,
		},
	}
}

func etcdCreateRow(txn *Transaction, k *common.Key, row *map[string]interface{}) error {
	key := k.String()
	val, err := makeValue(row)
	if err != nil {
		return err
	}

	etcdOp := clientv3.OpPut(key, val)
	txn.etcd.Then = append(txn.etcd.Then, etcdOp)

	etcdEvent := etcdEventCreate(key, val)
	txn.etcd.Events = append(txn.etcd.Events, etcdEvent)
	txn.etcd.Assert()

	return nil
}

func etcdModifyRow(txn *Transaction, k *common.Key, row *map[string]interface{}) error {
	key := k.String()
	val, err := makeValue(row)
	if err != nil {
		return err
	}

	etcdOp := clientv3.OpPut(key, val)
	txn.etcd.Then = append(txn.etcd.Then, etcdOp)

	prevVal, err := makeValue(txn.cache.Row(*k))
	if err != nil {
		return err
	}

	etcdEvent := etcdEventModify(key, val, prevVal)
	txn.etcd.Events = append(txn.etcd.Events, etcdEvent)
	txn.etcd.Assert()

	return nil
}

func etcdDeleteRow(txn *Transaction, k *common.Key) error {
	key := k.String()
	etcdOp := clientv3.OpDelete(key)
	txn.etcd.Then = append(txn.etcd.Then, etcdOp)

	prevVal, err := makeValue(txn.cache.Row(*k))
	if err != nil {
		return err
	}

	etcdEvent := etcdEventDelete(key, prevVal)
	txn.etcd.Events = append(txn.etcd.Events, etcdEvent)
	txn.etcd.Assert()

	return nil
}

func RowPrepare(tableSchema *libovsdb.TableSchema, mapUUID MapUUID, row *map[string]interface{}) error {
	err := tableSchema.Unmarshal(row)
	if err != nil {
		klog.Errorf("%s", err.Error())
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	err = mapUUID.ResolvRow(row)
	if err != nil {
		klog.Errorf("%s", err.Error())
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	err = tableSchema.Validate(row)
	if err != nil {
		klog.Errorf("%s", err.Error())
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	return nil
}

/* insert */
func preInsert(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	if ovsOp.UUIDName == nil {
		return nil
	}

	if ovsOp.UUIDName != nil {
		uuid := common.GenerateUUID()
		if ovsOp.UUID != nil {
			uuid = ovsOp.UUID.GoUUID
		}
		if _, ok := txn.mapUUID[*ovsOp.UUIDName]; ok {
			klog.Errorf("duplicate uuid-name: %s", *ovsOp.UUIDName)
			return errors.New(E_DUP_UUIDNAME)
		}
		txn.mapUUID.Set(*ovsOp.UUIDName, uuid)
	}

	key := common.NewTableKey(txn.request.DBName, *ovsOp.Table)
	etcdGetData(txn, &key)
	return nil
}

func doInsert(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		return errors.New(E_INTERNAL_ERROR)
	}

	uuid := common.GenerateUUID()

	if ovsOp.UUID != nil {
		uuid = ovsOp.UUID.GoUUID
	}

	if ovsOp.UUIDName != nil {
		uuid, err = txn.mapUUID.Get(*ovsOp.UUIDName)
		if err != nil {
			return err
		}
	}

	for uuid := range txn.cache.Table(txn.request.DBName, *ovsOp.Table) {
		if ovsOp.UUID != nil && uuid == ovsOp.UUID.GoUUID {
			klog.Errorf("Duplicate uuid: %s", *ovsOp.UUID)
			return errors.New(E_DUP_UUID)
		}
	}

	ovsResult.InitUUID(uuid)

	key := common.NewDataKey(txn.request.DBName, *ovsOp.Table, uuid)
	row := txn.cache.Row(key)
	*row = *ovsOp.Row
	txn.schemas.Default(txn.request.DBName, *ovsOp.Table, row)
	setRowUUID(row, uuid)

	err = RowPrepare(tableSchema, txn.mapUUID, ovsOp.Row)
	if err != nil {
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	return etcdCreateRow(txn, &key, row)
}

/* select */
func preSelect(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return etcdGetByWhere(txn, ovsOp, ovsResult)
}

func doSelect(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	ovsResult.InitRows()
	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		return errors.New(E_INTERNAL_ERROR)
	}

	for _, row := range txn.cache.Table(txn.request.DBName, *ovsOp.Table) {
		ok, err := isRowSelectedByWhere(tableSchema, txn.mapUUID, row, ovsOp.Where)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		resultRow, err := reduceRowByColumns(row, ovsOp.Columns)
		if err != nil {
			return err
		}
		ovsResult.AppendRows(*resultRow)
	}
	return nil
}

/* update */
func preUpdate(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return etcdGetByWhere(txn, ovsOp, ovsResult)
}

func doUpdate(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	ovsResult.InitCount()
	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		return errors.New(E_INTERNAL_ERROR)
	}
	for uuid, row := range txn.cache.Table(txn.request.DBName, *ovsOp.Table) {
		ok, err := isRowSelectedByWhere(tableSchema, txn.mapUUID, row, ovsOp.Where)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}

		err = RowPrepare(tableSchema, txn.mapUUID, ovsOp.Row)
		if err != nil {
			return err
		}

		err = RowUpdate(tableSchema, txn.mapUUID, row, ovsOp.Row)
		if err != nil {
			return err
		}
		key := common.NewDataKey(txn.request.DBName, *ovsOp.Table, uuid)
		*(txn.cache.Row(key)) = *row
		etcdModifyRow(txn, &key, row)
		ovsResult.IncrementCount()
	}
	return nil
}

/* mutate */
func preMutate(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return etcdGetByWhere(txn, ovsOp, ovsResult)
}

func doMutate(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	ovsResult.InitCount()
	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		return errors.New(E_INTERNAL_ERROR)
	}
	for uuid, row := range txn.cache.Table(txn.request.DBName, *ovsOp.Table) {
		ok, err := isRowSelectedByWhere(tableSchema, txn.mapUUID, row, ovsOp.Where)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		err = RowMutate(tableSchema, txn.mapUUID, row, ovsOp.Mutations)
		if err != nil {
			return err
		}
		key := common.NewDataKey(txn.request.DBName, *ovsOp.Table, uuid)
		*(txn.cache.Row(key)) = *row
		etcdModifyRow(txn, &key, row)
		ovsResult.IncrementCount()
	}
	return nil
}

/* delete */
func preDelete(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return etcdGetByWhere(txn, ovsOp, ovsResult)
}

func doDelete(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	ovsResult.InitCount()
	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		return errors.New(E_INTERNAL_ERROR)
	}
	for uuid, row := range txn.cache.Table(txn.request.DBName, *ovsOp.Table) {
		ok, err := isRowSelectedByWhere(tableSchema, txn.mapUUID, row, ovsOp.Where)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		key := common.NewDataKey(txn.request.DBName, *ovsOp.Table, uuid)
		etcdDeleteRow(txn, &key)
		ovsResult.IncrementCount()
	}
	return nil
}

/* wait */
func preWait(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	if ovsOp.Timeout == nil {
		klog.Errorf("missing timeout parameter")
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	if *ovsOp.Timeout != 0 {
		klog.Errorf("ignoring non-zero wait timeout %d", *ovsOp.Timeout)
	}
	return etcdGetByWhere(txn, ovsOp, ovsResult)
}

/* wait */
func doWait(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	if ovsOp.Table == nil {
		klog.Errorf("missing table parameter")
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	if ovsOp.Rows == nil {
		klog.Errorf("missing rows parameter")
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	if len(*ovsOp.Rows) == 0 {
		return nil
	}

	if ovsOp.Until == nil {
		klog.Errorf("missing until parameter")
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	tableSchema, err := txn.schemas.LookupTable(txn.request.DBName, *ovsOp.Table)
	if err != nil {
		klog.Errorf("%s", err)
		return errors.New(E_INTERNAL_ERROR)
	}

	var equal bool
	switch *ovsOp.Until {
	case FN_EQ:
		equal = true
	case FN_NE:
		equal = false
	default:
		klog.Errorf("wait: unsupported function %s", *ovsOp.Until)
		return errors.New(E_CONSTRAINT_VIOLATION)
	}

	for _, actual := range txn.cache.Table(txn.request.DBName, *ovsOp.Table) {
		ok, err := isRowSelectedByWhere(tableSchema, txn.mapUUID, actual, ovsOp.Where)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}

		if ovsOp.Columns != nil {
			actual, err = reduceRowByColumns(actual, ovsOp.Columns)
			if err != nil {
				klog.Errorf("wait: failed column reduction %s", err)
				return err
			}
		}

		for _, expected := range *ovsOp.Rows {
			err = RowPrepare(tableSchema, txn.mapUUID, &expected)
			if err != nil {
				return err
			}

			cond, err := isEqualRow(tableSchema, &expected, actual)
			if err != nil {
				klog.Errorf("wait: error in row compare %s", err)
				return err
			}
			if cond {
				if equal {
					return nil
				}
				klog.Errorf("wait: timed out")
				return errors.New(E_TIMEOUT)
			}
		}
	}

	if !equal {
		return nil
	}

	klog.Errorf("wait: timed out")
	return errors.New(E_TIMEOUT)
}

/* commit */
func preCommit(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	if ovsOp.Durable == nil {
		klog.Errorf("missing durable parameter")
		return errors.New(E_CONSTRAINT_VIOLATION)
	}
	if *ovsOp.Durable {
		klog.Errorf("do not support durable == true")
		return errors.New(E_NOT_SUPPORTED)
	}
	return nil
}

func doCommit(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return nil
}

/* abort */
func preAbort(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return errors.New(E_ABORTED)
}

func doAbort(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return nil
}

/* comment */
func preComment(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return nil
}

func doComment(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	timestamp := time.Now().Format(time.RFC3339)
	key := common.NewCommentKey(timestamp)
	comment := *ovsOp.Comment
	etcdOp := clientv3.OpPut(key.String(), comment)
	txn.etcd.Then = append(txn.etcd.Then, etcdOp)
	txn.etcd.Events = append(txn.etcd.Events, nil) /* so that events are aligned with then operations */
	txn.etcd.Assert()

	return nil
}

/* assert */
func preAssert(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return nil
}

func doAssert(txn *Transaction, ovsOp *libovsdb.Operation, ovsResult *libovsdb.OperationResult) error {
	return nil
}
