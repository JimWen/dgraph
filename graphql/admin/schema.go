/*
 * Copyright 2019 Dgraph Labs, Inc. and Contributors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package admin

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/golang/glog"

	dgoapi "github.com/dgraph-io/dgo/v2/protos/api"
	"github.com/dgraph-io/dgraph/edgraph"
	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/graphql/resolve"
	"github.com/dgraph-io/dgraph/graphql/schema"
	"github.com/dgraph-io/dgraph/x"
)

// A updateSchemaResolver serves as the mutation rewriter and executor in handling
// the updateGQLSchema mutation.
type updateSchemaResolver struct {
	admin *adminServer

	mutation schema.Mutation

	// schema that is generated from the mutation input
	newGQLSchema    schema.Schema
	newDgraphSchema string
	newSchema       gqlSchema

	// The underlying executor and rewriter that persist the schema into Dgraph as
	// GraphQL metadata
	baseAddRewriter      resolve.MutationRewriter
	baseMutationRewriter resolve.MutationRewriter
	baseMutationExecutor resolve.DgraphExecutor
}

type getSchemaResolver struct {
	admin *adminServer

	gqlQuery schema.Query
}

type updateGQLSchemaInput struct {
	Set gqlSchema `json:"set,omitempty"`
}

func (asr *updateSchemaResolver) Rewrite(
	m schema.Mutation) (*gql.GraphQuery, []*dgoapi.Mutation, error) {

	glog.Info("Got updateGQLSchema request")

	input, err := getSchemaInput(m)
	if err != nil {
		return nil, nil, err
	}

	asr.newSchema.Schema = input.Set.Schema
	schHandler, err := schema.NewHandler(asr.newSchema.Schema)
	if err != nil {
		return nil, nil, err
	}

	asr.mutation = m
	asr.newSchema.GeneratedSchema = schHandler.GQLSchema()
	asr.newGQLSchema, err = schema.FromString(asr.newSchema.GeneratedSchema)
	if err != nil {
		return nil, nil, err
	}

	asr.newDgraphSchema = schHandler.DGSchema()

	if asr.admin.schema.ID == "" {
		// There's never been a GraphQL schema in this Dgraph before so rewrite this into
		// an add
		m.SetArgTo(schema.InputArgName, map[string]interface{}{"schema": asr.newSchema.Schema})
		return asr.baseAddRewriter.Rewrite(m)
	}

	// there's already a value, just continue with the GraphQL update
	m.SetArgTo(schema.InputArgName,
		map[string]interface{}{
			"filter": map[string]interface{}{"ids": []interface{}{asr.admin.schema.ID}},
			"set":    map[string]interface{}{"schema": asr.newSchema.Schema},
		})
	return asr.baseMutationRewriter.Rewrite(m)
}

func (asr *updateSchemaResolver) FromMutationResult(
	mutation schema.Mutation,
	assigned map[string]string,
	result map[string]interface{}) (*gql.GraphQuery, error) {

	asr.mutation = mutation
	return nil, nil
}

func (asr *updateSchemaResolver) Execute(
	ctx context.Context,
	req *dgoapi.Request) (*dgoapi.Response, error) {

	asr.admin.mux.Lock()
	defer asr.admin.mux.Unlock()

	if req == nil || (req.Query == "" && len(req.Mutations) == 0) {
		// For schema updates, Execute will get called twice.  Once for the
		// mutation and once for the following query.  This is the query case.
		b, err := doQuery(asr.admin.schema, asr.mutation.SelectionSet()[0])
		return &dgoapi.Response{Json: b}, err
	}

	req.CommitNow = true
	resp, err := asr.baseMutationExecutor.Execute(ctx, req)
	if err != nil {
		return nil, err
	}

	if asr.admin.schema.ID == "" {
		// should only be 1 assigned, but we don't know the name
		for _, v := range resp.GetUids() {
			asr.newSchema.ID = v
		}
	} else {
		asr.newSchema.ID = asr.admin.schema.ID
	}

	_, err = (&edgraph.Server{}).Alter(ctx, &dgoapi.Operation{Schema: asr.newDgraphSchema,
		RunInBackground: false})

	if err != nil {
		return nil, schema.GQLWrapf(err,
			"succeeded in saving GraphQL schema but failed to alter Dgraph schema ")
	}

	asr.admin.resetSchema(asr.newGQLSchema)
	asr.admin.schema = asr.newSchema

	glog.Infof("Successfully loaded new GraphQL schema.  Serving New GraphQL API.")

	return resp, nil
}

func (asr *updateSchemaResolver) CommitOrAbort(ctx context.Context, tc *dgoapi.TxnContext) error {
	return nil
}

func (gsr *getSchemaResolver) Rewrite(ctx context.Context,
	gqlQuery schema.Query) (*gql.GraphQuery, error) {
	gsr.gqlQuery = gqlQuery
	return nil, nil
}

func (gsr *getSchemaResolver) Execute(
	ctx context.Context,
	req *dgoapi.Request) (*dgoapi.Response, error) {

	b, err := doQuery(gsr.admin.schema, gsr.gqlQuery)
	return &dgoapi.Response{Json: b}, err
}

func (gsr *getSchemaResolver) CommitOrAbort(ctx context.Context, tc *dgoapi.TxnContext) error {
	return nil
}

func doQuery(gql gqlSchema, field schema.Field) ([]byte, error) {
	var buf bytes.Buffer
	x.Check2(buf.WriteString(`{ "`))
	x.Check2(buf.WriteString(field.ResponseName()))
	if gql.ID == "" {
		x.Check2(buf.WriteString(`": null }`))
		return buf.Bytes(), nil
	}

	x.Check2(buf.WriteString(`": [{`))
	for i, sel := range field.SelectionSet() {
		var val []byte
		var err error
		switch sel.Name() {
		case "id":
			val, err = json.Marshal(gql.ID)
		case "schema":
			val, err = json.Marshal(gql.Schema)
		case "generatedSchema":
			val, err = json.Marshal(gql.GeneratedSchema)
		}
		x.Check2(val, err)

		if i != 0 {
			x.Check2(buf.WriteString(","))
		}
		x.Check2(buf.WriteString(`"`))
		x.Check2(buf.WriteString(sel.ResponseName()))
		x.Check2(buf.WriteString(`":`))
		x.Check2(buf.Write(val))
	}
	x.Check2(buf.WriteString("}]}"))

	return buf.Bytes(), nil
}

func getSchemaInput(m schema.Mutation) (*updateGQLSchemaInput, error) {
	inputArg := m.ArgValue(schema.InputArgName)
	inputByts, err := json.Marshal(inputArg)
	if err != nil {
		return nil, schema.GQLWrapf(err, "couldn't get input argument")
	}

	var input updateGQLSchemaInput
	err = json.Unmarshal(inputByts, &input)
	return &input, schema.GQLWrapf(err, "couldn't get input argument")
}
