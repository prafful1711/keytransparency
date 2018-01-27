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

// Package adminserver contains the KeyTransparencyAdmin implementation
package adminserver

import (
	"context"
	"fmt"
	"time"

	"github.com/golang/glog"
	"github.com/golang/protobuf/ptypes"

	"github.com/google/keytransparency/core/crypto/vrf/p256"
	"github.com/google/keytransparency/core/domain"
	"github.com/google/keytransparency/core/sequencer"
	"github.com/google/trillian/crypto/keys"
	"github.com/google/trillian/crypto/keys/der"
	"github.com/google/trillian/crypto/keyspb"
	"github.com/google/trillian/crypto/sigpb"
	"github.com/google/trillian/merkle/hashers"

	google_protobuf "github.com/golang/protobuf/ptypes/empty"
	pb "github.com/google/keytransparency/core/api/v1/keytransparency_proto"
	tpb "github.com/google/trillian"
	lclient "github.com/google/trillian/client"
)

var (
	vrfKeySpec = &keyspb.Specification{
		Params: &keyspb.Specification_EcdsaParams{
			EcdsaParams: &keyspb.Specification_ECDSA{
				Curve: keyspb.Specification_ECDSA_P256,
			},
		},
	}
	logArgs = &tpb.CreateTreeRequest{
		Tree: &tpb.Tree{
			DisplayName:        "KT SMH Log",
			TreeState:          tpb.TreeState_ACTIVE,
			TreeType:           tpb.TreeType_LOG,
			HashStrategy:       tpb.HashStrategy_OBJECT_RFC6962_SHA256,
			SignatureAlgorithm: sigpb.DigitallySigned_ECDSA,
			HashAlgorithm:      sigpb.DigitallySigned_SHA256,
			MaxRootDuration:    ptypes.DurationProto(0 * time.Millisecond),
		},
		KeySpec: &keyspb.Specification{
			Params: &keyspb.Specification_EcdsaParams{
				EcdsaParams: &keyspb.Specification_ECDSA{
					Curve: keyspb.Specification_ECDSA_P256,
				},
			},
		},
	}
	mapArgs = &tpb.CreateTreeRequest{
		Tree: &tpb.Tree{
			DisplayName:        "KT Map",
			TreeState:          tpb.TreeState_ACTIVE,
			TreeType:           tpb.TreeType_MAP,
			HashStrategy:       tpb.HashStrategy_CONIKS_SHA512_256,
			SignatureAlgorithm: sigpb.DigitallySigned_ECDSA,
			HashAlgorithm:      sigpb.DigitallySigned_SHA256,
			MaxRootDuration:    ptypes.DurationProto(0 * time.Millisecond),
		},
		KeySpec: &keyspb.Specification{
			Params: &keyspb.Specification_EcdsaParams{
				EcdsaParams: &keyspb.Specification_ECDSA{
					Curve: keyspb.Specification_ECDSA_P256,
				},
			},
		},
	}
)

// Server implements pb.KeyTransparencyAdminServer
type Server struct {
	tlog     tpb.TrillianLogClient
	tmap     tpb.TrillianMapClient
	logAdmin tpb.TrillianAdminClient
	mapAdmin tpb.TrillianAdminClient
	domains  domain.Storage
	keygen   keys.ProtoGenerator
}

// New returns a KeyTransparencyAdmin implementation.
func New(tlog tpb.TrillianLogClient,
	tmap tpb.TrillianMapClient,
	logAdmin, mapAdmin tpb.TrillianAdminClient,
	domains domain.Storage,
	keygen keys.ProtoGenerator) *Server {
	return &Server{
		tlog:     tlog,
		tmap:     tmap,
		logAdmin: logAdmin,
		mapAdmin: mapAdmin,
		domains:  domains,
		keygen:   keygen,
	}
}

// ListDomains produces a list of the configured domains
func (s *Server) ListDomains(ctx context.Context, in *pb.ListDomainsRequest) (*pb.ListDomainsResponse, error) {
	domains, err := s.domains.List(ctx, in.GetShowDeleted())
	if err != nil {
		return nil, err
	}

	resp := make([]*pb.Domain, 0, len(domains))
	for _, d := range domains {
		info, err := s.fetchDomain(ctx, d)
		if err != nil {
			return nil, err
		}
		resp = append(resp, info)

	}
	return &pb.ListDomainsResponse{
		Domains: resp,
	}, nil
}

// fetchDomain converts an adminstorage.Domain object into a pb.Domain object
// by fetching the relevant info from Trillian.
func (s *Server) fetchDomain(ctx context.Context, d *domain.Domain) (*pb.Domain, error) {
	logTree, err := s.logAdmin.GetTree(ctx, &tpb.GetTreeRequest{TreeId: d.LogID})
	if err != nil {
		return nil, err
	}
	mapTree, err := s.mapAdmin.GetTree(ctx, &tpb.GetTreeRequest{TreeId: d.MapID})
	if err != nil {
		return nil, err
	}
	return &pb.Domain{
		DomainId:    d.DomainID,
		Log:         logTree,
		Map:         mapTree,
		Vrf:         d.VRF,
		MinInterval: ptypes.DurationProto(d.MinInterval),
		MaxInterval: ptypes.DurationProto(d.MaxInterval),
		Deleted:     d.Deleted,
	}, nil
}

// GetDomain retrieves the domain info for a given domain.
func (s *Server) GetDomain(ctx context.Context, in *pb.GetDomainRequest) (*pb.Domain, error) {
	domain, err := s.domains.Read(ctx, in.GetDomainId(), in.GetShowDeleted())
	if err != nil {
		return nil, err
	}
	info, err := s.fetchDomain(ctx, domain)
	if err != nil {
		return nil, err
	}
	return info, nil
}

// CreateDomain reachs out to Trillian to produce new trees.
func (s *Server) CreateDomain(ctx context.Context, in *pb.CreateDomainRequest) (*pb.Domain, error) {
	// TODO(gbelvin): Test whether the domain exists before creating trees.

	// Generate VRF key.
	wrapped, err := s.keygen(ctx, vrfKeySpec)
	if err != nil {
		return nil, fmt.Errorf("keygen: %v", err)
	}
	vrfPriv, err := p256.NewFromWrappedKey(ctx, wrapped)
	if err != nil {
		return nil, fmt.Errorf("NewFromWrappedKey(): %v", err)
	}
	vrfPublicPB, err := der.ToPublicProto(vrfPriv.Public())
	if err != nil {
		return nil, err
	}

	// Create Trillian keys.
	logTreeArgs := *logArgs
	logTreeArgs.Tree.Description = fmt.Sprintf("KT domain %s's SMH Log", in.GetDomainId())
	logTree, err := s.logAdmin.CreateTree(ctx, &logTreeArgs)
	if err != nil {
		return nil, fmt.Errorf("CreateTree(log): %v", err)
	}
	mapTreeArgs := *mapArgs
	mapTreeArgs.Tree.Description = fmt.Sprintf("KT domain %s's Map", in.GetDomainId())
	mapTree, err := s.mapAdmin.CreateTree(ctx, &mapTreeArgs)
	if err != nil {
		return nil, fmt.Errorf("CreateTree(map): %v", err)
	}
	minInterval, err := ptypes.Duration(in.MinInterval)
	if err != nil {
		return nil, fmt.Errorf("Duration(%v): %v", in.MinInterval, err)
	}
	maxInterval, err := ptypes.Duration(in.MaxInterval)
	if err != nil {
		return nil, fmt.Errorf("Duration(%v): %v", in.MaxInterval, err)
	}

	// Initialize log with first map root.
	if err := s.initialize(ctx, logTree, mapTree); err != nil {
		return nil, fmt.Errorf("initialize of log %v and map %v failed: %v",
			logTree.TreeId, mapTree.TreeId, err)
	}

	if err := s.domains.Write(ctx, &domain.Domain{
		DomainID:    in.GetDomainId(),
		MapID:       mapTree.TreeId,
		LogID:       logTree.TreeId,
		VRF:         vrfPublicPB,
		VRFPriv:     wrapped,
		MinInterval: minInterval,
		MaxInterval: maxInterval,
	}); err != nil {
		return nil, fmt.Errorf("adminstorage.Write(): %v", err)
	}
	glog.Infof("Created domain %v", in.GetDomainId())
	return &pb.Domain{
		DomainId: in.GetDomainId(),
		Log:      logTree,
		Map:      mapTree,
		Vrf:      vrfPublicPB,
	}, nil
}

// initialize inserts the first (empty) SignedMapRoot into the log if it is empty.
// This keeps the log leaves in-sync with the map which starts off with an
// empty log root at map revision 0.
func (s *Server) initialize(ctx context.Context, logTree, mapTree *tpb.Tree) error {
	logID := logTree.GetTreeId()
	mapID := mapTree.GetTreeId()

	logClient, err := s.newLogClient(logTree)
	if err != nil {
		return fmt.Errorf("could not create log client: %v", err)
	}

	logRoot, err := s.tlog.GetLatestSignedLogRoot(ctx,
		&tpb.GetLatestSignedLogRootRequest{LogId: logID})
	if err != nil {
		return fmt.Errorf("GetLatestSignedLogRoot(%v): %v", logID, err)
	}
	mapRoot, err := s.tmap.GetSignedMapRoot(ctx,
		&tpb.GetSignedMapRootRequest{MapId: mapID})
	if err != nil {
		return fmt.Errorf("GetSignedMapRoot(%v): %v", mapID, err)
	}

	// If the tree is empty and the map is empty,
	// add the empty map root to the log.
	if logRoot.GetSignedLogRoot().GetTreeSize() == 0 &&
		mapRoot.GetMapRoot().GetMapRevision() == 0 {
		glog.Infof("Initializing Trillian Log %v with empty map root", logID)

		// Blocking add leaf
		smrData, err := sequencer.CanonicalSignedMapRoot(mapRoot.GetMapRoot())
		if err != nil {
			return err
		}
		if err := logClient.AddLeaf(ctx, smrData); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) newLogClient(config *tpb.Tree) (*lclient.LogClient, error) {
	// Log Hasher.
	logHasher, err := hashers.NewLogHasher(config.GetHashStrategy())
	if err != nil {
		return nil, fmt.Errorf("Failed creating LogHasher: %v", err)
	}

	// Log Key
	logPubKey, err := der.UnmarshalPublicKey(config.GetPublicKey().GetDer())
	if err != nil {
		return nil, fmt.Errorf("Failed parsing Log public key: %v", err)
	}

	logID := config.GetTreeId()

	return lclient.New(logID, s.tlog, logHasher, logPubKey), nil
}

// DeleteDomain marks a domain as deleted, but does not immediately delete it.
func (s *Server) DeleteDomain(ctx context.Context, in *pb.DeleteDomainRequest) (*google_protobuf.Empty, error) {
	if err := s.domains.SetDelete(ctx, in.GetDomainId(), true); err != nil {
		return nil, err
	}
	return &google_protobuf.Empty{}, nil
}

// UndeleteDomain reactivates a deleted domain - provided that UndeleteDomain is called sufficiently soon after DeleteDomain.
func (s *Server) UndeleteDomain(ctx context.Context, in *pb.UndeleteDomainRequest) (*google_protobuf.Empty, error) {
	if err := s.domains.SetDelete(ctx, in.GetDomainId(), false); err != nil {
		return nil, err
	}
	return &google_protobuf.Empty{}, nil
}
