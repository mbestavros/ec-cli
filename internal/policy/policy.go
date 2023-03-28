// Copyright 2022 Red Hat, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package policy

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	ecc "github.com/hacbs-contract/enterprise-contract-controller/api/v1alpha1"
	"github.com/hashicorp/go-multierror"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/rekor"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	ociremote "github.com/sigstore/cosign/v2/pkg/oci/remote"
	cosignSig "github.com/sigstore/cosign/v2/pkg/signature"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	sigstoreSig "github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/tuf"
	log "github.com/sirupsen/logrus"

	"github.com/hacbs-contract/ec-cli/internal/kubernetes"
	e "github.com/hacbs-contract/ec-cli/pkg/error"
)

const (
	Now           = "now"
	AtAttestation = "attestation"
	DateFormat    = "2006-01-02"
)

var (
	PO001 = e.NewError("PO001", "Invalid policy time argument", e.ErrorExitStatus)
)

// allows controlling time in tests
var now = time.Now

type Policy interface {
	PublicKeyPEM() ([]byte, error)
	CheckOpts() (*cosign.CheckOpts, error)
	WithSpec(spec ecc.EnterpriseContractPolicySpec) Policy
	Spec() ecc.EnterpriseContractPolicySpec
	EffectiveTime() time.Time
	AttestationTime(time.Time)
}

type policy struct {
	ecc.EnterpriseContractPolicySpec
	checkOpts       *cosign.CheckOpts
	choosenTime     string
	effectiveTime   *time.Time
	attestationTime *time.Time
}

// PublicKeyPEM returns the PublicKey in PEM format.
func (p *policy) PublicKeyPEM() ([]byte, error) {
	if p.checkOpts == nil || p.checkOpts.SigVerifier == nil {
		return nil, errors.New("no check options or sig verifier configured")
	}
	pk, err := p.checkOpts.SigVerifier.PublicKey()
	if err != nil {
		return nil, err
	}
	return cryptoutils.MarshalPublicKeyToPEM(pk)
}

func (p *policy) CheckOpts() (*cosign.CheckOpts, error) {
	if p.checkOpts == nil {
		return nil, errors.New("no check options configured")
	}
	return p.checkOpts, nil
}

func (p *policy) Spec() ecc.EnterpriseContractPolicySpec {
	return p.EnterpriseContractPolicySpec
}

// NewOfflinePolicy construct and return a new instance of Policy that is used
// in offline scenarios, i.e. without cluster or specific services access, and
// no signature verification being performed.
func NewOfflinePolicy(ctx context.Context, effectiveTime string) (Policy, error) {
	if efn, err := parseEffectiveTime(effectiveTime); err == nil {
		return &policy{
			effectiveTime: efn,
			choosenTime:   effectiveTime,
			checkOpts:     &cosign.CheckOpts{},
		}, nil
	} else {
		return nil, err
	}
}

// NewPolicy construct and return a new instance of Policy.
//
// The policyRef parameter is expected to be either a JSON-encoded instance of
// EnterpriseContractPolicySpec, or reference to the location of the EnterpriseContractPolicy
// resource in Kubernetes using the format: [namespace/]name
//
// If policyRef is blank, an empty EnterpriseContractPolicySpec is used.
//
// rekorUrl and publicKey provide a mechanism to overwrite the attributes, of same name, in the
// EnterpriseContractPolicySpec.
//
// The public key is resolved as part of object construction. If the public key is a reference
// to a kubernetes resource, for example, the cluster will be contacted.
func NewPolicy(ctx context.Context, policyRef, rekorUrl, publicKey, effectiveTime string) (Policy, error) {
	p := policy{
		choosenTime: effectiveTime,
	}

	if policyRef == "" {
		log.Debug("Using an empty EnterpriseContractPolicy")
		// Default to an empty policy instead of returning an error because the required
		// values, e.g. PublicKey, may be provided via other means, e.g. publicKey param.
	} else if strings.Contains(policyRef, ":") { // Should detect JSON or YAML objects 🤞
		log.Debug("Read EnterpriseContractPolicy as YAML")
		if err := yaml.Unmarshal([]byte(policyRef), &p); err != nil {
			log.Debugf("Problem parsing EnterpriseContractPolicy Spec from %q", policyRef)
			return nil, fmt.Errorf("unable to parse EnterpriseContractPolicy Spec: %w", err)
		}
	} else {
		log.Debug("Read EnterpriseContractPolicy as k8s resource")
		k8s, err := kubernetes.NewClient(ctx)
		if err != nil {
			log.Debug("Failed to initialize Kubernetes client")
			return nil, fmt.Errorf("cannot initialize Kubernetes client: %w", err)
		}
		log.Debug("Initialized Kubernetes client")

		ecp, err := k8s.FetchEnterpriseContractPolicy(ctx, policyRef)
		if err != nil {
			log.Debug("Failed to fetch the enterprise contract policy from the cluster!")
			return nil, fmt.Errorf("unable to fetch EnterpriseContractPolicy: %w", err)
		}
		p.EnterpriseContractPolicySpec = ecp.Spec
	}

	if rekorUrl != "" && rekorUrl != p.RekorUrl {
		p.RekorUrl = rekorUrl
		log.Debugf("Updated rekor URL in policy to %q", rekorUrl)
	}

	if publicKey != "" && publicKey != p.PublicKey {
		p.PublicKey = publicKey
		log.Debugf("Updated public key in policy to %q", publicKey)
	}

	if p.PublicKey == "" {
		return nil, errors.New("policy must provide a public key")
	}

	if efn, err := parseEffectiveTime(effectiveTime); err != nil {
		return nil, err
	} else {
		p.effectiveTime = efn
	}

	if opts, err := checkOpts(ctx, &p); err != nil {
		return nil, err
	} else {
		p.checkOpts = opts
	}

	return &p, nil
}

func (p *policy) WithSpec(spec ecc.EnterpriseContractPolicySpec) Policy {
	p.EnterpriseContractPolicySpec = spec

	return p
}

func (p *policy) AttestationTime(attestationTime time.Time) {
	p.attestationTime = &attestationTime
	if p.choosenTime == AtAttestation {
		p.effectiveTime = &attestationTime
	}
}

func (p policy) EffectiveTime() time.Time {
	if p.effectiveTime == nil {
		now := now().UTC()
		log.Debugf("No effective time choosen using current time: %s", now.Format(time.RFC3339))
		p.effectiveTime = &now
	} else {
		log.Debugf("Using effective time: %s", p.effectiveTime.Format(time.RFC3339))
	}

	return *p.effectiveTime
}

func isNow(choosenTime string) bool {
	return strings.EqualFold(choosenTime, Now)
}

func parseEffectiveTime(choosenTime string) (*time.Time, error) {
	switch {
	case isNow(choosenTime):
		now := now().UTC()
		log.Debugf("Chosen to use effective time of `now`, using current time %s", now.Format(time.RFC3339))
		return &now, nil
	case strings.EqualFold(choosenTime, AtAttestation):
		log.Debugf("Chosen to use effective time of `attestation`")
		return nil, nil
	default:
		var err error
		if when, err := time.Parse(time.RFC3339, choosenTime); err == nil {
			log.Debugf("Using provided effective time %s", when.Format(time.RFC3339))
			whenUTC := when.UTC()
			return &whenUTC, nil
		}

		log.Debugf("Unable to parse provided effective time `%s` using RFC3339", choosenTime)
		errs := multierror.Append(err)

		if when, err := time.Parse(DateFormat, choosenTime); err == nil {
			log.Debugf("Using provided effective time %s", when.Format(time.RFC3339))
			whenUTC := when.UTC()
			return &whenUTC, nil
		}
		log.Debugf("Unable to provided effective time string `%s` using %s format", choosenTime, DateFormat)
		errs = multierror.Append(errs, err)

		return nil, PO001.CausedBy(errs)
	}
}

// checkOpts returns an instance based on attributes of the Policy.
func checkOpts(ctx context.Context, p *policy) (*cosign.CheckOpts, error) {
	opts := cosign.CheckOpts{}

	verifier, err := signatureVerifier(ctx, p)
	if err != nil {
		return nil, err
	}
	opts.SigVerifier = verifier
	progressChan := make(chan v1.Update)
	opts.RegistryClientOpts = append(opts.RegistryClientOpts, ociremote.WithRemoteOptions(remote.WithProgress(progressChan)))

	go printProgress(progressChan)

	if p.RekorUrl == "" {
		opts.IgnoreTlog = true
	} else {
		rekorClient, err := rekor.NewClient(p.RekorUrl)
		if err != nil {
			log.Debugf("Problem creating a rekor client using url %q", p.RekorUrl)
			return nil, err
		}

		opts.RekorClient = rekorClient
		log.Debug("Rekor client created")

		// TODO the Rekor public key and entry id should originate in the policy
		rekorPublicKeyPEM := os.Getenv("REKOR_PUBLIC_KEY")
		if rekorPublicKeyPEM != "" {
			rekorPublicKey, err := cryptoutils.UnmarshalPEMToPublicKey([]byte(rekorPublicKeyPEM))
			if err != nil {
				return nil, err
			}
			rekorPublicKeyBytes, err := x509.MarshalPKIXPublicKey(rekorPublicKey)
			if err != nil {
				return nil, err
			}
			digest := sha256.Sum256(rekorPublicKeyBytes)
			logId := hex.EncodeToString(digest[:])

			opts.RekorPubKeys = &cosign.TrustedTransparencyLogPubKeys{
				Keys: map[string]cosign.TransparencyLogPubKey{
					logId: {
						PubKey: rekorPublicKey,
						Status: tuf.Active,
					},
				},
			}
		}
	}

	return &opts, nil
}

func printProgress(progress chan v1.Update) {

	incomplete := true

	log.Error("starting output loop")
	for incomplete {
		select {
		case currentProgress := <-progress:
			log.Error("current progress is: ", currentProgress)
		default:
			log.Error("no progress")
		}
		time.Sleep(1 * time.Second)
	}

}

type signatureClient interface {
	publicKeyFromKeyRef(context.Context, string) (sigstoreSig.Verifier, error)
}

type cosignClient struct{}

func (c *cosignClient) publicKeyFromKeyRef(ctx context.Context, publicKey string) (sigstoreSig.Verifier, error) {
	return cosignSig.PublicKeyFromKeyRef(ctx, publicKey)
}

type contextKey string

const signatureClientContextKey contextKey = "ec.policy.signature.client"

func withSignatureClient(ctx context.Context, client signatureClient) context.Context {
	return context.WithValue(ctx, signatureClientContextKey, client)
}

func newSignatureClient(ctx context.Context) signatureClient {
	client, ok := ctx.Value(signatureClientContextKey).(signatureClient)
	if ok && client != nil {
		return client
	}

	return &cosignClient{}
}

// signatureVerifier creates a new instance based on the PublicKey from the Policy.
func signatureVerifier(ctx context.Context, p *policy) (sigstoreSig.Verifier, error) {
	publicKey := p.PublicKey

	if strings.Contains(publicKey, "-----BEGIN PUBLIC KEY-----") {
		verifier, err := cosignSig.LoadPublicKeyRaw([]byte(publicKey), crypto.SHA256)
		if err != nil {
			return nil, err
		}
		return verifier, nil
	}

	verifier, err := newSignatureClient(ctx).publicKeyFromKeyRef(ctx, publicKey)
	if err != nil {
		return nil, err
	}
	return verifier, nil
}
