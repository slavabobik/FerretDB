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

// Package cursor provides access to cursor registry.
//
// The implementation of the cursor and registry is quite complicated and entangled.
// That's because there are many cases when cursor / iterator / underlying database connection
// must be closed to free resources, including when no handler and backend code is running;
// for example, when the client disconnects between `getMore` commands.
// At the same time, we want to shift complexity away from the handler and from backend implementations
// because they are already quite complex.
// The current design enables ease of use at the expense of the implementation complexity.
package cursor

import (
	"sync"
	"time"

	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/resource"
)

// Cursor allows clients to iterate over a result set.
//
// It implements types.DocumentsIterator interface by wrapping another iterator with documents
// with additional metadata and registration in the registry.
//
// Closing the cursor removes it from the registry.
type Cursor struct {
	// the order of fields is weird to make the struct smaller due to alignment

	created      time.Time
	iter         types.DocumentsIterator
	r            *Registry
	token        *resource.Token
	closed       chan struct{}
	DB           string
	Collection   string
	Username     string
	ID           int64
	closeOnce    sync.Once
	showRecordID bool
}

// newCursor creates a new cursor.
func newCursor(id int64, params *NewParams, r *Registry) *Cursor {
	c := &Cursor{
		ID:           id,
		DB:           params.DB,
		Collection:   params.Collection,
		Username:     params.Username,
		showRecordID: params.ShowRecordID,
		iter:         params.Iter,
		r:            r,
		created:      time.Now(),
		closed:       make(chan struct{}),
		token:        resource.NewToken(),
	}

	resource.Track(c, c.token)

	return c
}

// Next implements types.DocumentsIterator interface.
func (c *Cursor) Next() (struct{}, *types.Document, error) {
	zero, doc, err := c.iter.Next()
	if doc != nil {
		if c.showRecordID {
			doc.Set("$recordId", doc.RecordID())
		}
	}

	return zero, doc, err
}

// Close implements types.DocumentsIterator interface.
func (c *Cursor) Close() {
	c.closeOnce.Do(func() {
		c.iter.Close()
		c.iter = nil

		c.r.delete(c)

		close(c.closed)

		resource.Untrack(c, c.token)
	})
}

// check interfaces
var (
	_ types.DocumentsIterator = (*Cursor)(nil)
)
