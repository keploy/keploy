package hooks

import (
	"fmt"

	"github.com/hashicorp/go-memdb"
)

type localDb struct {
	db *memdb.MemDB
}

func NewLocalDb(schemaMap map[string]map[string]string) (*localDb, error) {
	schema := NewLocalDbSchema(schemaMap)
	db, err := memdb.NewMemDB(schema)
	if err != nil {
		return nil, fmt.Errorf("error while creating new memdb: %v", err)
	}
	return &localDb{
		db: db,
	}, nil
}

func NewLocalDbSchema(tableAndIndexMap map[string]map[string]string) *memdb.DBSchema {

	tableSchemaMap := make(map[string]*memdb.TableSchema, len(tableAndIndexMap))
	for tableName, indexMap := range tableAndIndexMap {
		indexSchemaMap := make(map[string]*memdb.IndexSchema, len(indexMap))
		for k, v := range indexMap {
			indexSchema := &memdb.IndexSchema{
				Name:    k,
				Unique:  true,
				Indexer: &memdb.StringFieldIndex{Field: v},
			}
			indexSchemaMap[k] = indexSchema
		}
		tableSchema := &memdb.TableSchema{
			Name:    tableName,
			Indexes: indexSchemaMap,
		}
		tableSchemaMap[tableName] = tableSchema
	}

	schema := &memdb.DBSchema{
		Tables: tableSchemaMap,
	}

	return schema
}

func (ldb *localDb) insert(tableName string, obj interface{}) error {
	txn := ldb.db.Txn(true)
	err := txn.Insert(tableName, obj)
	if err != nil {
		txn.Abort()
		return fmt.Errorf("error while inserting obj into %v table: %v", tableName, err)
	}
	txn.Commit()
	return nil
}

func (ldb *localDb) delete(tableName string, obj interface{}) (bool, error) {
	txn := ldb.db.Txn(true)
	err := txn.Delete(tableName, obj)
	if err != nil {
		txn.Abort()
		if err == memdb.ErrNotFound {
			return false, nil
		}
		return false, fmt.Errorf("error while deleting obj from %v table: %v", tableName, err)
	}
	txn.Commit()
	return true, nil
}

func (ldb *localDb) deleteAll(tableName string, index string) error {
	txn := ldb.db.Txn(true)
	_, err := txn.DeleteAll(tableName, index)
	if err != nil {
		txn.Abort()
		return fmt.Errorf("error while deleting all objects from %v table: %v", tableName, err)
	}
	txn.Commit()
	return err
}

func (ldb *localDb) getAll(tableName string, index string) (memdb.ResultIterator, error) {
	txn := ldb.db.Txn(false)
	resultIterator, err := txn.Get(tableName, index)
	if err != nil {
		txn.Abort()
		return resultIterator, fmt.Errorf("error while getting all obj from %v table: %v", tableName, err)
	}
	txn.Commit()
	return resultIterator, nil
}
