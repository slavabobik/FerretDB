// Copyright 2021 FerretDB Inc.
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

package postgresql

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/FerretDB/FerretDB/internal/backends"
	"github.com/FerretDB/FerretDB/internal/backends/postgresql/metadata"
	"github.com/FerretDB/FerretDB/internal/backends/postgresql/metadata/pool"
	"github.com/FerretDB/FerretDB/internal/handler/sjson"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
)

// collection implements backends.Collection interface.
type collection struct {
	r      *metadata.Registry
	dbName string
	name   string
}

// newCollection creates a new Collection.
func newCollection(r *metadata.Registry, dbName, name string) backends.Collection {
	return backends.CollectionContract(&collection{
		r:      r,
		dbName: dbName,
		name:   name,
	})
}

// Query implements backends.Collection interface.
func (c *collection) Query(ctx context.Context, params *backends.QueryParams) (*backends.QueryResult, error) {
	p, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if p == nil {
		return &backends.QueryResult{
			Iter: newQueryIterator(ctx, nil, params.OnlyRecordIDs),
		}, nil
	}

	meta, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if meta == nil {
		return &backends.QueryResult{
			Iter: newQueryIterator(ctx, nil, params.OnlyRecordIDs),
		}, nil
	}

	q := prepareSelectClause(&selectParams{
		Schema:        c.dbName,
		Table:         meta.TableName,
		Comment:       params.Comment,
		Capped:        meta.Capped(),
		OnlyRecordIDs: params.OnlyRecordIDs,
	})

	var placeholder metadata.Placeholder

	where, args, err := prepareWhereClause(&placeholder, params.Filter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	q += where

	sort, sortArgs := prepareOrderByClause(&placeholder, params.Sort, meta.Capped())
	q += sort

	args = append(args, sortArgs...)

	if params.Limit != 0 {
		q += fmt.Sprintf(` LIMIT %s`, placeholder.Next())
		args = append(args, params.Limit)
	}

	rows, err := p.Query(ctx, q, args...)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &backends.QueryResult{
		Iter: newQueryIterator(ctx, rows, params.OnlyRecordIDs),
	}, nil
}

// InsertAll implements backends.Collection interface.
func (c *collection) InsertAll(ctx context.Context, params *backends.InsertAllParams) (*backends.InsertAllResult, error) {
	if _, err := c.r.CollectionCreate(ctx, &metadata.CollectionCreateParams{
		DBName: c.dbName,
		Name:   c.name,
	}); err != nil {
		return nil, lazyerrors.Error(err)
	}

	p, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	meta, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	err = pool.InTransaction(ctx, p, func(tx pgx.Tx) error {
		// TODO https://github.com/FerretDB/FerretDB/issues/3708
		const batchSize = 100

		var batch []*types.Document
		docs := params.Docs

		for len(docs) > 0 {
			i := min(batchSize, len(docs))
			batch, docs = docs[:i], docs[i:]

			var q string
			var args []any

			q, args, err = prepareInsertStatement(c.dbName, meta.TableName, meta.Capped(), batch)
			if err != nil {
				return lazyerrors.Error(err)
			}

			if _, err = tx.Exec(ctx, q, args...); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
					return backends.NewError(backends.ErrorCodeInsertDuplicateID, err)
				}

				return lazyerrors.Error(err)
			}
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return new(backends.InsertAllResult), nil
}

// UpdateAll implements backends.Collection interface.
func (c *collection) UpdateAll(ctx context.Context, params *backends.UpdateAllParams) (*backends.UpdateAllResult, error) {
	p, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var res backends.UpdateAllResult
	if p == nil {
		return &res, nil
	}

	meta, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if meta == nil {
		return &res, nil
	}

	q := fmt.Sprintf(
		`UPDATE %s SET %s = $1 WHERE %s = $2`,
		pgx.Identifier{c.dbName, meta.TableName}.Sanitize(),
		metadata.DefaultColumn,
		metadata.IDColumn,
	)

	err = pool.InTransaction(ctx, p, func(tx pgx.Tx) error {
		for _, doc := range params.Docs {
			var b []byte
			if b, err = sjson.Marshal(doc); err != nil {
				return lazyerrors.Error(err)
			}

			id, _ := doc.Get("_id")
			must.NotBeZero(id)

			arg := must.NotFail(sjson.MarshalSingleValue(id))

			var tag pgconn.CommandTag
			if tag, err = tx.Exec(ctx, q, b, arg); err != nil {
				return lazyerrors.Error(err)
			}

			res.Updated += int32(tag.RowsAffected())
		}

		return nil
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &res, nil
}

// DeleteAll implements backends.Collection interface.
func (c *collection) DeleteAll(ctx context.Context, params *backends.DeleteAllParams) (*backends.DeleteAllResult, error) {
	p, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if p == nil {
		return &backends.DeleteAllResult{Deleted: 0}, nil
	}

	meta, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if meta == nil {
		return &backends.DeleteAllResult{Deleted: 0}, nil
	}

	var column string
	var placeholders []string
	var args []any

	if params.RecordIDs == nil {
		var placeholder metadata.Placeholder
		placeholders = make([]string, len(params.IDs))
		args = make([]any, len(params.IDs))

		for i, id := range params.IDs {
			placeholders[i] = placeholder.Next()
			args[i] = string(must.NotFail(sjson.MarshalSingleValue(id)))
		}

		column = metadata.IDColumn
	} else {
		var placeholder metadata.Placeholder
		placeholders = make([]string, len(params.RecordIDs))
		args = make([]any, len(params.RecordIDs))

		for i, id := range params.RecordIDs {
			placeholders[i] = placeholder.Next()
			args[i] = id
		}

		column = metadata.RecordIDColumn
	}

	q := fmt.Sprintf(`DELETE FROM %s WHERE %s IN (%s)`,
		pgx.Identifier{c.dbName, meta.TableName}.Sanitize(),
		column,
		strings.Join(placeholders, ", "),
	)

	res, err := p.Exec(ctx, q, args...)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &backends.DeleteAllResult{
		Deleted: int32(res.RowsAffected()),
	}, nil
}

// Explain implements backends.Collection interface.
func (c *collection) Explain(ctx context.Context, params *backends.ExplainParams) (*backends.ExplainResult, error) {
	p, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return &backends.ExplainResult{
			QueryPlanner: must.NotFail(types.NewDocument()),
		}, nil
	}

	if p == nil {
		return &backends.ExplainResult{
			QueryPlanner: must.NotFail(types.NewDocument()),
		}, nil
	}

	meta, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if meta == nil {
		return &backends.ExplainResult{
			QueryPlanner: must.NotFail(types.NewDocument()),
		}, nil
	}

	res := new(backends.ExplainResult)

	opts := &selectParams{
		Schema: c.dbName,
		Table:  meta.TableName,
		Capped: meta.Capped(),
	}

	q := `EXPLAIN (VERBOSE true, FORMAT JSON) ` + prepareSelectClause(opts)

	var placeholder metadata.Placeholder

	where, args, err := prepareWhereClause(&placeholder, params.Filter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	res.FilterPushdown = where != ""

	q += where

	sort, sortArgs := prepareOrderByClause(&placeholder, params.Sort, meta.Capped())
	q += sort

	args = append(args, sortArgs...)
	res.SortPushdown = sort != ""

	if params.Limit != 0 {
		q += fmt.Sprintf(` LIMIT %s`, placeholder.Next())
		args = append(args, params.Limit)
		res.LimitPushdown = true
	}

	var b []byte
	if err = p.QueryRow(ctx, q, args...).Scan(&b); err != nil {
		return nil, lazyerrors.Error(err)
	}

	queryPlan, err := unmarshalExplain(b)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	res.QueryPlanner = queryPlan

	return res, nil
}

// Stats implements backends.Collection interface.
func (c *collection) Stats(ctx context.Context, params *backends.CollectionStatsParams) (*backends.CollectionStatsResult, error) {
	p, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if p == nil {
		return nil, backends.NewError(
			backends.ErrorCodeCollectionDoesNotExist,
			lazyerrors.Errorf("no ns %s.%s", c.dbName, c.name),
		)
	}

	coll, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if coll == nil {
		return nil, backends.NewError(
			backends.ErrorCodeCollectionDoesNotExist,
			lazyerrors.Errorf("no ns %s.%s", c.dbName, c.name),
		)
	}

	stats, err := collectionsStats(ctx, p, c.dbName, []*metadata.Collection{coll}, params.Refresh)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	indexMap := map[string]string{}
	for _, index := range coll.Indexes {
		indexMap[index.PgIndex] = index.Name
	}

	q := `
		SELECT
			indexname,
			pg_relation_size(quote_ident(schemaname)|| '.' || quote_ident(indexname), 'main')
		FROM pg_indexes
		WHERE schemaname = $1 AND tablename IN ($2)
		`

	rows, err := p.Query(ctx, q, c.dbName, coll.TableName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	defer rows.Close()

	indexSizes := make([]backends.IndexSize, len(indexMap))
	var i int

	for rows.Next() {
		var name string
		var size int64

		if err = rows.Scan(&name, &size); err != nil {
			return nil, lazyerrors.Error(err)
		}

		indexName, ok := indexMap[name]
		if !ok {
			// new index have been created since fetching metadata
			continue
		}

		indexSizes[i] = backends.IndexSize{
			Name: indexName,
			Size: size,
		}
		i++
	}

	if rows.Err() != nil {
		return nil, lazyerrors.Error(rows.Err())
	}

	return &backends.CollectionStatsResult{
		CountDocuments:  stats.countDocuments,
		SizeTotal:       stats.sizeTables + stats.sizeIndexes,
		SizeIndexes:     stats.sizeIndexes,
		SizeCollection:  stats.sizeTables,
		SizeFreeStorage: stats.sizeFreeStorage,
		IndexSizes:      indexSizes,
	}, nil
}

// Compact implements backends.Collection interface.
func (c *collection) Compact(ctx context.Context, params *backends.CompactParams) (*backends.CompactResult, error) {
	db, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if db == nil {
		return nil, backends.NewError(
			backends.ErrorCodeDatabaseDoesNotExist,
			lazyerrors.Errorf("no ns %s.%s", c.dbName, c.name),
		)
	}

	coll, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if coll == nil {
		return nil, backends.NewError(
			backends.ErrorCodeCollectionDoesNotExist,
			lazyerrors.Errorf("no ns %s.%s", c.dbName, c.name),
		)
	}

	q := "VACUUM ANALYZE "
	if params != nil && params.Full {
		q = "VACUUM FULL ANALYZE "
	}
	q += pgx.Identifier{c.dbName, coll.TableName}.Sanitize()

	if _, err = db.Exec(ctx, q); err != nil {
		return nil, lazyerrors.Error(err)
	}

	return new(backends.CompactResult), nil
}

// ListIndexes implements backends.Collection interface.
func (c *collection) ListIndexes(ctx context.Context, params *backends.ListIndexesParams) (*backends.ListIndexesResult, error) {
	db, err := c.r.DatabaseGetExisting(ctx, c.dbName)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if db == nil {
		return nil, backends.NewError(
			backends.ErrorCodeCollectionDoesNotExist,
			lazyerrors.Errorf("no ns %s.%s", c.dbName, c.name),
		)
	}

	coll, err := c.r.CollectionGet(ctx, c.dbName, c.name)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if coll == nil {
		return nil, backends.NewError(
			backends.ErrorCodeCollectionDoesNotExist,
			lazyerrors.Errorf("no ns %s.%s", c.dbName, c.name),
		)
	}

	res := backends.ListIndexesResult{
		Indexes: make([]backends.IndexInfo, len(coll.Indexes)),
	}

	for i, index := range coll.Indexes {
		res.Indexes[i] = backends.IndexInfo{
			Name:   index.Name,
			Unique: index.Unique,
			Key:    make([]backends.IndexKeyPair, len(index.Key)),
		}

		for j, key := range index.Key {
			res.Indexes[i].Key[j] = backends.IndexKeyPair{
				Field:      key.Field,
				Descending: key.Descending,
			}
		}
	}

	sort.Slice(res.Indexes, func(i, j int) bool {
		return res.Indexes[i].Name < res.Indexes[j].Name
	})

	return &res, nil
}

// CreateIndexes implements backends.Collection interface.
func (c *collection) CreateIndexes(ctx context.Context, params *backends.CreateIndexesParams) (*backends.CreateIndexesResult, error) { //nolint:lll // for readability
	indexes := make([]metadata.IndexInfo, len(params.Indexes))
	for i, index := range params.Indexes {
		indexes[i] = metadata.IndexInfo{
			Name:   index.Name,
			Key:    make([]metadata.IndexKeyPair, len(index.Key)),
			Unique: index.Unique,
		}

		for j, key := range index.Key {
			indexes[i].Key[j] = metadata.IndexKeyPair{
				Field:      key.Field,
				Descending: key.Descending,
			}
		}
	}

	err := c.r.IndexesCreate(ctx, c.dbName, c.name, indexes)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return new(backends.CreateIndexesResult), nil
}

// DropIndexes implements backends.Collection interface.
func (c *collection) DropIndexes(ctx context.Context, params *backends.DropIndexesParams) (*backends.DropIndexesResult, error) {
	err := c.r.IndexesDrop(ctx, c.dbName, c.name, params.Indexes)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return new(backends.DropIndexesResult), nil
}

// check interfaces
var (
	_ backends.Collection = (*collection)(nil)
)
