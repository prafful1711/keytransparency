// Copyright 2017 Google Inc. All Rights Reserved.
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

package entry

import (
	"fmt"

	"github.com/golang/protobuf/proto"
	"github.com/google/keytransparency/core/crypto/commitments"
	"github.com/google/keytransparency/core/crypto/signatures"
	"github.com/google/keytransparency/core/mutator"

	"github.com/google/trillian/crypto/keyspb"
	"github.com/google/trillian/crypto/sigpb"

	"github.com/benlaurie/objecthash/go/objecthash"

	pb "github.com/google/keytransparency/core/api/v1/keytransparency_proto"
)

var nilHash, _ = objecthash.ObjectHash(nil)

// Mutation provides APIs for manipulating entries.
type Mutation struct {
	DomainID, AppID, UserID string
	data, nonce             []byte

	prevEntry *pb.Entry
	entry     *pb.Entry
}

// NewMutation creates a mutation object from a previous value which can be modified.
// To create a new value:
// - Create a new mutation for a user starting with the previous value with NewMutation.
// - Change the value with SetCommitment and ReplaceAuthorizedKeys.
// - Finalize the changes and create the mutation with SerializeAndSign.
func NewMutation(index []byte, domainID, appID, userID string) *Mutation {
	return &Mutation{
		DomainID: domainID,
		AppID:    appID,
		UserID:   userID,
		entry: &pb.Entry{
			Index:    index,
			Previous: nilHash[:],
		},
	}
}

// SetPrevious sets the previous hash.
// If copyPrevious is true, AuthorizedKeys and Commitment are also copied.
func (m *Mutation) SetPrevious(oldValue []byte, copyPrevious bool) error {
	prevEntry, err := FromLeafValue(oldValue)
	if err != nil {
		return err
	}

	pej, err := objecthash.CommonJSONify(prevEntry)
	if err != nil {
		return err
	}
	hash, err := objecthash.ObjectHash(pej)
	if err != nil {
		return err
	}

	m.prevEntry = prevEntry
	m.entry.Previous = hash[:]
	if copyPrevious {
		m.entry.AuthorizedKeys = prevEntry.GetAuthorizedKeys()
		m.entry.Commitment = prevEntry.GetCommitment()
	}
	return nil
}

// SetCommitment updates entry to be a commitment to data.
func (m *Mutation) SetCommitment(data []byte) error {
	// Commit to profile.
	commitmentNonce, err := commitments.GenCommitmentKey()
	if err != nil {
		return err
	}
	m.data = data
	m.nonce = commitmentNonce
	m.entry.Commitment = commitments.Commit(m.UserID, m.AppID, data, commitmentNonce)
	return nil
}

// ReplaceAuthorizedKeys sets authorized keys to pubkeys.
// pubkeys must contain at least one key.
func (m *Mutation) ReplaceAuthorizedKeys(pubkeys []*keyspb.PublicKey) error {
	if got, want := len(pubkeys), 1; got < want {
		return mutator.ErrMissingKey
	}
	m.entry.AuthorizedKeys = pubkeys
	return nil
}

// SerializeAndSign produces the mutation.
func (m *Mutation) SerializeAndSign(signers []signatures.Signer, trustedTreeSize int64) (*pb.UpdateEntryRequest, error) {
	mutation, err := m.sign(signers)
	if err != nil {
		return nil, err
	}

	// Check authorization.
	skv := *mutation
	skv.Signatures = nil
	if err := verifyKeys(m.prevEntry.GetAuthorizedKeys(),
		m.entry.GetAuthorizedKeys(),
		skv,
		mutation.GetSignatures()); err != nil {
		return nil, fmt.Errorf("verifyKeys(prevauth: %v, newauth: %v, sig: %v): %v",
			len(m.prevEntry.GetAuthorizedKeys()), len(m.entry.GetAuthorizedKeys()), len(mutation.GetSignatures()), err)
	}

	// Sanity check the mutation's correctness.
	if _, err := New().Mutate(m.prevEntry, mutation); err != nil {
		return nil, fmt.Errorf("presign mutation check: %v", err)
	}

	return &pb.UpdateEntryRequest{
		DomainId:      m.DomainID,
		UserId:        m.UserID,
		AppId:         m.AppID,
		FirstTreeSize: trustedTreeSize,
		EntryUpdate: &pb.EntryUpdate{
			Mutation: mutation,
			Committed: &pb.Committed{
				Key:  m.nonce,
				Data: m.data,
			},
		},
	}, nil
}

// Sign produces the mutation
func (m *Mutation) sign(signers []signatures.Signer) (*pb.Entry, error) {
	m.entry.Signatures = nil
	sigs := make(map[string]*sigpb.DigitallySigned)
	for _, signer := range signers {
		sig, err := signer.Sign(m.entry)
		if err != nil {
			return nil, err
		}
		sigs[signer.KeyID()] = sig
	}

	m.entry.Signatures = sigs
	return m.entry, nil
}

// EqualsRequested verifies that an update was successfully applied.
// Returns nil if newLeaf is equal to the entry in this mutation.
func (m *Mutation) EqualsRequested(leafValue proto.Message) bool {
	// TODO(gbelvin): Figure out reliable object comparison.
	// Mutations are no longer stable serialized byte slices, so we need to
	// use an equality operation on the proto itself.
	return proto.Equal(leafValue, m.entry)
}

// EqualsPrevious returns true if the leafValue is equal to
// the value of entry at the time this mutation was made.
func (m *Mutation) EqualsPrevious(leafValue proto.Message) bool {
	return proto.Equal(leafValue, m.prevEntry)
}
