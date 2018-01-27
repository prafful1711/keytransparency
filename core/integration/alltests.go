// Copyright 2018 Google Inc. All Rights Reserved.
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

// Package integration exports a set of unit tests that can be run by impl/integration
// or any other specific instantiation of KeyTransparency.
package integration

import (
	"context"
	"testing"

	"github.com/google/keytransparency/core/client/grpcc"
	"github.com/google/keytransparency/core/mutator"

	pb "github.com/google/keytransparency/core/api/v1/keytransparency_proto"
)

// Env holds a complete testing environment for end-to-end tests.
type Env struct {
	Client   *grpcc.Client
	Cli      pb.KeyTransparencyClient
	Domain   *pb.Domain
	Receiver mutator.Receiver
}

// NamedTestFn is a binding between a readable test name (used for a Go subtest)
// and a function that performs the test, given a test environment.
type NamedTestFn struct {
	Name string
	Fn   func(context.Context, *Env, *testing.T)
}

// AllTests contains all the integration tests.
// Be sure to extend this when additional tests are added.
// This is done so that tests can be run in different environments in a portable way.
var AllTests = []NamedTestFn{
	// Client Tests
	{"TestEmptyGetAndUpdate", TestEmptyGetAndUpdate},
	{"TestUpdateValidation", TestUpdateValidation},
	{"TestListHistory", TestListHistory},
	// Monitor Tests
	{"TestMonitor", TestMonitor},
}
