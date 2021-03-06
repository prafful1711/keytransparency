// Copyright 2016 Google Inc. All Rights Reserved.
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

// Package grpcc is a client for communicating with the Key Server.  It wraps
// the gRPC apis in a rpc system neutral interface and verifies all responses.
package grpcc

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/google/keytransparency/core/client/kt"
	"github.com/google/keytransparency/core/crypto/signatures"
	"github.com/google/keytransparency/core/crypto/vrf"
	"github.com/google/keytransparency/core/crypto/vrf/p256"
	"github.com/google/keytransparency/core/mutator"
	"github.com/google/keytransparency/core/mutator/entry"

	"github.com/google/trillian"
	"github.com/google/trillian/client"
	"github.com/google/trillian/crypto/keys/der"
	"github.com/google/trillian/crypto/keyspb"
	"github.com/google/trillian/merkle/hashers"

	"google.golang.org/grpc"

	pb "github.com/google/keytransparency/core/api/v1/keytransparency_proto"
)

const (
	// Each page contains pageSize profiles. Each profile contains multiple
	// keys. Assuming 2 keys per profile (each of size 2048-bit), a page of
	// size 16 will contain about 8KB of data.
	pageSize = 16
	// TODO: Public keys of trusted monitors.
)

var (
	// ErrRetry occurs when an update request has been submitted, but the
	// results of the update are not visible on the server yet. The client
	// must retry until the request is visible.
	ErrRetry = errors.New("update not present on server yet")
	// ErrIncomplete occurs when the server indicates that requested epochs
	// are not available.
	ErrIncomplete = errors.New("incomplete account history")
	// Vlog is the verbose logger. By default it outputs to /dev/null.
	Vlog = log.New(ioutil.Discard, "", 0)
)

// Client is a helper library for issuing updates to the key server.
// Client Responsibilities
// - Trust Model:
// - - Trusted Monitors
// - - Verify last X days
// - Gossip - What is the current value of the root?
// -  - Gossip advancement: advance state between current and server.
// - Sender queries - Do queries match up against the gossip root?
// - - List trusted monitors.
// - Key Owner
// - - Periodically query own keys. Do they match the private keys I have?
// - - Sign key update requests.
type Client struct {
	cli        pb.KeyTransparencyClient
	domainID   string
	kt         *kt.Verifier
	mutator    mutator.Func
	RetryCount int
	RetryDelay time.Duration
	trusted    trillian.SignedLogRoot
}

// NewFromConfig creates a new client from a config
func NewFromConfig(ktClient pb.KeyTransparencyClient, config *pb.Domain) (*Client, error) {
	// Log Hasher.
	logHasher, err := hashers.NewLogHasher(config.GetLog().GetHashStrategy())
	if err != nil {
		return nil, fmt.Errorf("Failed creating LogHasher: %v", err)
	}

	// Log Key
	logPubKey, err := der.UnmarshalPublicKey(config.GetLog().GetPublicKey().GetDer())
	if err != nil {
		return nil, fmt.Errorf("Failed parsing Log public key: %v", err)
	}

	// Map Hasher
	mapHasher, err := hashers.NewMapHasher(config.GetMap().GetHashStrategy())
	if err != nil {
		return nil, fmt.Errorf("Failed creating MapHasher: %v", err)
	}

	// Map Key
	mapPubKey, err := der.UnmarshalPublicKey(config.GetMap().GetPublicKey().GetDer())
	if err != nil {
		return nil, fmt.Errorf("Failed parsing Map public key: %v", err)
	}

	// VRF key
	vrfPubKey, err := p256.NewVRFVerifierFromRawKey(config.GetVrf().GetDer())
	if err != nil {
		return nil, fmt.Errorf("Error parsing vrf public key: %v", err)
	}

	// TODO(gbelvin): set retry delay.
	logVerifier := client.NewLogVerifier(logHasher, logPubKey)
	return New(ktClient, config.DomainId, vrfPubKey, mapPubKey, mapHasher, logVerifier), nil
}

// New creates a new client.
func New(ktClient pb.KeyTransparencyClient,
	domainID string,
	vrf vrf.PublicKey,
	mapPubKey crypto.PublicKey,
	mapHasher hashers.MapHasher,
	logVerifier client.LogVerifier) *Client {
	return &Client{
		cli:        ktClient,
		domainID:   domainID,
		kt:         kt.New(vrf, mapHasher, mapPubKey, logVerifier),
		mutator:    entry.New(),
		RetryCount: 1,
		RetryDelay: 3 * time.Second,
	}
}

// GetEntry returns an entry if it exists, and nil if it does not.
func (c *Client) GetEntry(ctx context.Context, userID, appID string, opts ...grpc.CallOption) ([]byte, *trillian.SignedMapRoot, error) {
	e, err := c.cli.GetEntry(ctx, &pb.GetEntryRequest{
		DomainId:      c.domainID,
		UserId:        userID,
		AppId:         appID,
		FirstTreeSize: c.trusted.TreeSize,
	}, opts...)
	if err != nil {
		return nil, nil, err
	}

	if err := c.kt.VerifyGetEntryResponse(ctx, c.domainID, appID, userID, &c.trusted, e); err != nil {
		return nil, nil, err
	}

	// Empty case.
	if e.GetCommitted() == nil {
		return nil, e.GetSmr(), nil
	}

	return e.GetCommitted().GetData(), e.GetSmr(), nil
}

func min(x, y int32) int32 {
	if x < y {
		return x
	}
	return y
}

// ListHistory returns a list of profiles starting and ending at given epochs.
// It also filters out all identical consecutive profiles.
func (c *Client) ListHistory(ctx context.Context, userID, appID string, start, end int64, opts ...grpc.CallOption) (map[*trillian.SignedMapRoot][]byte, error) {
	if start < 0 {
		return nil, fmt.Errorf("start=%v, want >= 0", start)
	}
	var currentProfile []byte
	profiles := make(map[*trillian.SignedMapRoot][]byte)
	epochsReceived := int64(0)
	epochsWant := end - start + 1
	for epochsReceived < epochsWant {
		resp, err := c.cli.ListEntryHistory(ctx, &pb.ListEntryHistoryRequest{
			DomainId: c.domainID,
			UserId:   userID,
			AppId:    appID,
			Start:    start,
			PageSize: min(int32((end-start)+1), pageSize),
		}, opts...)
		if err != nil {
			return nil, err
		}
		epochsReceived += int64(len(resp.GetValues()))

		for i, v := range resp.GetValues() {
			Vlog.Printf("Processing entry for %v, epoch %v", userID, start+int64(i))
			err = c.kt.VerifyGetEntryResponse(ctx, c.domainID, appID, userID, &c.trusted, v)
			if err != nil {
				return nil, err
			}

			// Compress profiles that are equal through time.  All
			// nil profiles before the first profile are ignored.
			profile := v.GetCommitted().GetData()
			if bytes.Equal(currentProfile, profile) {
				continue
			}

			// Append the slice and update currentProfile.
			profiles[v.GetSmr()] = profile
			currentProfile = profile
		}
		if resp.NextStart == 0 {
			break // No more data.
		}
		start = resp.NextStart // Fetch the next block of results.
	}

	if epochsReceived < epochsWant {
		return nil, ErrIncomplete
	}

	return profiles, nil
}

// Update creates an UpdateEntryRequest for a user, attempt to submit it multiple
// times depending on RetryCount.
func (c *Client) Update(ctx context.Context, appID, userID string, profileData []byte,
	signers []signatures.Signer, authorizedKeys []*keyspb.PublicKey,
	opts ...grpc.CallOption) (*entry.Mutation, error) {
	getResp, err := c.cli.GetEntry(ctx, &pb.GetEntryRequest{
		DomainId:      c.domainID,
		UserId:        userID,
		AppId:         appID,
		FirstTreeSize: c.trusted.TreeSize,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("GetEntry(%v): %v", userID, err)
	}
	Vlog.Printf("Got current entry...")

	if err := c.kt.VerifyGetEntryResponse(ctx, c.domainID, appID, userID, &c.trusted, getResp); err != nil {
		return nil, fmt.Errorf("VerifyGetEntryResponse(): %v", err)
	}

	m, err := c.kt.NewMutation(c.domainID, appID, userID, profileData, authorizedKeys,
		getResp.GetVrfProof(), getResp.GetLeafProof().GetLeaf().GetLeafValue())
	if err != nil {
		return nil, fmt.Errorf("CreateUpdateEntryRequest: %v", err)
	}

	err = c.Retry(ctx, m, signers, opts...)
	// Retry submitting until an inclusion proof is returned.
	for i := 0; err == ErrRetry && i < c.RetryCount; i++ {
		time.Sleep(c.RetryDelay)
		err = c.Retry(ctx, m, signers, opts...)
	}
	return m, err
}

// Retry takes take a mutation, signs, and sends it again, and updates the back pointer with the current leaf value.
func (c *Client) Retry(ctx context.Context, m *entry.Mutation, signers []signatures.Signer, opts ...grpc.CallOption) error {
	req, err := m.SerializeAndSign(signers, c.trusted.TreeSize)
	if err != nil {
		return fmt.Errorf("SerializeAndSign(): %v", err)
	}

	Vlog.Printf("Sending Update request...")
	updateResp, err := c.cli.UpdateEntry(ctx, req, opts...)
	if err != nil {
		return fmt.Errorf("cli.UpdateEntry(): %v", err)
	}
	Vlog.Printf("Got current entry...")

	// Validate response.
	if err := c.kt.VerifyGetEntryResponse(ctx, c.domainID, req.AppId, req.UserId, &c.trusted, updateResp.GetProof()); err != nil {
		return fmt.Errorf("VerifyGetEntryResponse(): %v", err)
	}

	cntLeaf := updateResp.GetProof().GetLeafProof().GetLeaf().GetLeafValue()
	equal, err := m.Check(cntLeaf)
	if err != nil {
		return fmt.Errorf("mutation.Check(): %v", err)
	}
	if err := m.SetPrevious(cntLeaf, false); err != nil {
		return fmt.Errorf("mutation.SetPrevious(): %v", err)
	}
	if !equal {
		return ErrRetry
	}
	return nil
}
