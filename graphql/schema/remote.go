/*
 * Copyright 2020 Dgraph Labs, Inc. and Contributors
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

package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/dgraph-io/dgraph/x"
	"github.com/pkg/errors"
	"github.com/vektah/gqlparser/v2/ast"
)

// returnType is const key to represent graphql return. This key can't be used by user because,
// TypeKey uses restricted delimiter to form the key.
var returnType = string(x.TypeKey("graphql-return"))

// introspectRemoteSchema introspectes remote schema
func introspectRemoteSchema(url string) (*IntrospectedSchema, error) {
	param := &Request{
		Query: introspectionQuery,
	}

	body, err := json.Marshal(param)

	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest(http.MethodPost, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	result := &IntrospectedSchema{}

	return result, json.Unmarshal(body, result)
}

const introspectionQuery = `
	query {
	  __schema {
		queryType { name }
		mutationType { name }
		subscriptionType { name }
		types {
		  ...FullType
		}
		directives {
		  name
		  locations
		  args {
			...InputValue
		  }
		}
	  }
	}
	fragment FullType on __Type {
	  kind
	  name
	  fields(includeDeprecated: true) {
		name
		args {
		  ...InputValue
		}
		type {
		  ...TypeRef
		}
		isDeprecated
		deprecationReason
	  }
	  inputFields {
		...InputValue
	  }
	  interfaces {
		...TypeRef
	  }
	  enumValues(includeDeprecated: true) {
		name
		isDeprecated
		deprecationReason
	  }
	  possibleTypes {
		...TypeRef
	  }
	}
	fragment InputValue on __InputValue {
	  name
	  type { ...TypeRef }
	  defaultValue
	}
	fragment TypeRef on __Type {
	  kind
	  name
	  ofType {
		kind
		name
		ofType {
		  kind
		  name
		  ofType {
			kind
			name
			ofType {
			  kind
			  name
			  ofType {
				kind
				name
				ofType {
				  kind
				  name
				  ofType {
					kind
					name
				  }
				}
			  }
			}
		  }
		}
	  }
	}
  `

// remoteGraphqlMetadata represents the minimal set of data that is required to validate the graphql
// given in @custom->http->graphql with the remote server
type remoteGraphqlMetadata struct {
	// parentType is the type which contains the field on which @custom is applied
	parentType *ast.Definition
	// parentField refers to the field on which @custom is applied
	parentField *ast.FieldDefinition
	// graphqlOpDef is the Operation Definition for the operation given in @custom->http->graphql
	// The operation can only be a query or mutation
	graphqlOpDef *ast.OperationDefinition
	// url is the url of remote graphql endpoint
	url string
	// schema is the parsed schema given by the user
	schema *ast.Schema
}

// validates the graphql given in @custom->http->graphql by introspecting remote schema.
// It assumes that the graphql syntax is correct, only remote validation is needed.
func validateRemoteGraphql(metadata *remoteGraphqlMetadata) error {
	remoteIntrospection, err := introspectRemoteSchema(metadata.url)
	if err != nil {
		return err
	}

	var remoteQueryTypename string
	operationType := metadata.graphqlOpDef.Operation
	switch operationType {
	case "query":
		remoteQueryTypename = remoteIntrospection.Data.Schema.QueryType.Name
	case "mutation":
		remoteQueryTypename = remoteIntrospection.Data.Schema.MutationType.Name
	default:
		// this case is not possible as we are validating the operation to be query/mutation in
		// @custom directive validation
		return errors.Errorf("found %s operation, it can only have query/mutation.", operationType)
	}

	var introspectedRemoteQuery *GqlField
	givenQuery := metadata.graphqlOpDef.SelectionSet[0].(*ast.Field)
	for _, typ := range remoteIntrospection.Data.Schema.Types {
		if typ.Name != remoteQueryTypename {
			continue
		}
		for _, remoteQuery := range typ.Fields {
			if remoteQuery.Name == givenQuery.Name {
				introspectedRemoteQuery = &remoteQuery
				break
			}
		}
		if introspectedRemoteQuery != nil {
			break
		}
	}

	// check whether given query/mutation is present in remote schema
	if introspectedRemoteQuery == nil {
		return errors.Errorf("given %s: %s is not present in remote schema.",
			operationType, givenQuery.Name)
	}

	// check whether the return type of remote query is same as the required return type
	// TODO: need to check whether same will work for @custom on fields which have batch operation
	expectedReturnType := introspectedRemoteQuery.Type.String()
	gotReturnType := metadata.parentField.Type.String()
	if expectedReturnType != gotReturnType {
		return errors.Errorf("given %s: %s: return type mismatch; expected: %s, got: %s.",
			operationType, givenQuery.Name, expectedReturnType, gotReturnType)
	}

	givenQryArgDefs, givenQryArgVals := getGivenQueryArgsAsMap(givenQuery, metadata.parentField,
		metadata.parentType)
	remoteQryArgDefs, remoteQryRequiredArgs := getRemoteQueryArgsAsMap(introspectedRemoteQuery)

	// check whether args of given query/mutation match the args of remote query/mutation
	for givenArgName, givenArgDef := range givenQryArgDefs {
		remoteArgDef, ok := remoteQryArgDefs[givenArgName]
		if !ok {
			return errors.Errorf("given %s: %s: arg %s not present in remote %s.", operationType,
				givenQuery.Name, givenArgName, operationType)
		}
		if givenArgDef == nil {
			return errors.Errorf("given %s: %s: variable %s is missing in given context.",
				operationType, givenQuery.Name, givenQryArgVals[givenArgName])
		}
		expectedArgType := remoteArgDef.Type.String()
		gotArgType := givenArgDef.Type.String()
		if expectedArgType != gotArgType {
			return errors.Errorf("given %s: %s: type mismatch for variable %s; expected: %s, "+
				"got: %s.", operationType, givenQuery.Name, givenQryArgVals[givenArgName],
				expectedArgType, gotArgType)
		}
	}

	// check all non-null args required by remote query/mutation are present in given query/mutation
	for _, remoteArgName := range remoteQryRequiredArgs {
		_, ok := givenQryArgVals[remoteArgName]
		if !ok {
			return errors.Errorf("given %s: %s: required arg %s is missing.", operationType,
				givenQuery.Name, remoteArgName)
		}
	}

	// Add the return type to the remoteQryArgDefs to further expand the nested
	// types to validate against the local schema.
	remoteQryArgDefs[returnType] = Args{Type: introspectedRemoteQuery.Type}
	expandedRemoteTypes, err := expandArgs(remoteQryArgDefs, remoteIntrospection)
	if err != nil {
		return err
	}
	// Type check the expanded local type with the local schema.
	for typeName, fields := range expandedRemoteTypes {
		localType, ok := metadata.schema.Types[typeName]
		if !ok {
			return errors.Errorf(
				"Unable to find remote type %s in the local schema",
				typeName,
			)
		}
		for _, field := range fields {
			localField := localType.Fields.ForName(field.Name)
			if localField == nil {
				return errors.Errorf(
					"%s field for the remote type %s is not present in the local type %s",
					field.Name, localType.Name, localType.Name,
				)
			}
			if localField.Type.String() != field.Type.String() {
				return errors.Errorf(
					"expected type for the field %s is %s but got %s in type %s",
					field.Name,
					field.Type.String(),
					localField.Type.String(),
					typeName,
				)
			}
		}
	}

	return nil
}

type expandArgParams struct {
	expandedTypes      map[string]struct{}
	introspectedSchema *IntrospectedSchema
	typesToFields      map[string][]GqlField
}

func expandArgRecursively(arg string, param *expandArgParams) error {
	_, alreadyExpanded := param.expandedTypes[arg]
	if alreadyExpanded {
		return nil
	}
	// We're marking this to avoid recursive expansion.
	param.expandedTypes[arg] = struct{}{}
	typeFound := false
	for _, inputType := range param.introspectedSchema.Data.Schema.Types {
		if inputType.Name == arg {
			typeFound = true
			param.typesToFields[inputType.Name] = inputType.Fields
			// Expand the non scalar types.
			for _, field := range inputType.Fields {
				_, ok := graphqlScalarType[field.Type.Name]
				if !ok {
					// expand this field.
					err := expandArgRecursively(field.Type.NamedType(), param)
					if err != nil {
						return err
					}
				}
			}
			// expand input fields as well.
			param.typesToFields[inputType.Name] = append(param.typesToFields[inputType.Name],
				inputType.InputFields...)
			for _, field := range inputType.InputFields {
				_, ok := graphqlScalarType[field.Type.NamedType()]
				if !ok {
					// expand this field.
					err := expandArgRecursively(field.Type.NamedType(), param)
					if err != nil {
						return err
					}
				}
			}
		}
	}
	if !typeFound {
		return errors.Errorf("Unable to find the type %s on the remote schema", arg)
	}
	return nil
}

// expandArgs will expand the nested type into flat structure. For eg. Country have a filed with
// field of states of type State is expanded as Country and State. Scalar fields won't be expanded.
// It also expands deep nested types.
func expandArgs(argToVal map[string]Args,
	introspectedSchema *IntrospectedSchema) (map[string][]GqlField, error) {

	param := &expandArgParams{
		expandedTypes:      make(map[string]struct{}, 0),
		typesToFields:      make(map[string][]GqlField),
		introspectedSchema: introspectedSchema,
	}
	// Expand the types that are required to do a query.
	for _, typeTobeExpanded := range argToVal {
		_, ok := graphqlScalarType[typeTobeExpanded.Type.NamedType()]
		if ok {
			continue
		}
		err := expandArgRecursively(typeTobeExpanded.Type.NamedType(), param)
		if err != nil {
			return nil, err
		}
	}
	return param.typesToFields, nil
}

// getGivenQueryArgsAsMap returns following maps:
// 1. arg name -> *ast.ArgumentDefinition
// 2. arg name -> argument value (i.e., variable like $id)
func getGivenQueryArgsAsMap(givenQuery *ast.Field, parentField *ast.FieldDefinition,
	parentType *ast.Definition) (map[string]*ast.ArgumentDefinition, map[string]string) {
	argDefMap := make(map[string]*ast.ArgumentDefinition)
	argValMap := make(map[string]string)

	if parentType.Name == "Query" || parentType.Name == "Mutation" {
		parentFieldArgMap := getFieldArgDefsAsMap(parentField)
		for _, arg := range givenQuery.Arguments {
			varName := arg.Value.String()
			argDefMap[arg.Name] = parentFieldArgMap[varName[1:]]
			argValMap[arg.Name] = varName
		}
	} else {
		// TODO: handle @custom graphql validation for fields here
	}
	return argDefMap, argValMap
}

func getFieldArgDefsAsMap(fieldDef *ast.FieldDefinition) map[string]*ast.ArgumentDefinition {
	argMap := make(map[string]*ast.ArgumentDefinition)
	for _, v := range fieldDef.Arguments {
		argMap[v.Name] = v
	}
	return argMap
}

// getRemoteQueryArgsAsMap returns following things:
// 1. map of arg name -> Argument Definition in Gql introspection response format
// 2. list of arg name for NON_NULL args
func getRemoteQueryArgsAsMap(remoteQuery *GqlField) (map[string]Args, []string) {
	argDefMap := make(map[string]Args)
	requiredArgs := make([]string, 0)

	for _, arg := range remoteQuery.Args {
		argDefMap[arg.Name] = arg
		if arg.Type.Kind == "NON_NULL" {
			requiredArgs = append(requiredArgs, arg.Name)
		}
	}
	return argDefMap, requiredArgs
}

type IntrospectedSchema struct {
	Data Data `json:"data"`
}
type IntrospectionQueryType struct {
	Name string `json:"name"`
}
type GqlType struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	OfType *GqlType `json:"ofType"`
}
type GqlField struct {
	Name              string      `json:"name"`
	Args              []Args      `json:"args"`
	Type              *GqlType    `json:"type"`
	IsDeprecated      bool        `json:"isDeprecated"`
	DeprecationReason interface{} `json:"deprecationReason"`
}
type Types struct {
	Kind          string        `json:"kind"`
	Name          string        `json:"name"`
	Fields        []GqlField    `json:"fields"`
	InputFields   []GqlField    `json:"inputFields"`
	Interfaces    []interface{} `json:"interfaces"`
	EnumValues    interface{}   `json:"enumValues"`
	PossibleTypes interface{}   `json:"possibleTypes"`
}
type Args struct {
	Name         string      `json:"name"`
	Type         *GqlType    `json:"type"`
	DefaultValue interface{} `json:"defaultValue"`
}
type Directives struct {
	Name      string   `json:"name"`
	Locations []string `json:"locations"`
	Args      []Args   `json:"args"`
}
type IntrospectionSchema struct {
	QueryType        IntrospectionQueryType `json:"queryType"`
	MutationType     IntrospectionQueryType `json:"mutationType"`
	SubscriptionType IntrospectionQueryType `json:"subscriptionType"`
	Types            []Types                `json:"types"`
	Directives       []Directives           `json:"directives"`
}
type Data struct {
	Schema IntrospectionSchema `json:"__schema"`
}

func (t *GqlType) String() string {
	if t == nil {
		return ""
	}
	// refer http://spec.graphql.org/June2018/#sec-Type-Kinds
	// it confirms, if type kind is LIST or NON_NULL all other fields except ofType will be
	// null, so there won't be any name at that level.
	switch t.Kind {
	case "LIST":
		return fmt.Sprintf("[%s]", t.OfType.String())
	case "NON_NULL":
		return fmt.Sprintf("%s!", t.OfType.String())
	default:
		return t.Name
	}
}

func (t *GqlType) NamedType() string {
	if t.Name != "" {
		return t.Name
	}
	return t.OfType.NamedType()
}