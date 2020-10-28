/*
Copyright 2020 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auth

import (
	"context"
	"time"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/tlsca"

	"github.com/gravitational/trace"
)

// GenerateDatabaseCert generates client certificate used by a database
// service to authenticate with the database instance.
func (s *Server) GenerateDatabaseCert(ctx context.Context, req *proto.DatabaseCertRequest) (*proto.DatabaseCertResponse, error) {
	csr, err := tlsca.ParseCertificateRequestPEM(req.CSR)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	clusterName, err := s.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	hostCA, err := s.GetCertAuthority(services.CertAuthID{
		Type:       services.HostCA,
		DomainName: clusterName.GetClusterName(),
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsCA, err := hostCA.TLSCA()
	if err != nil {
		return nil, trace.Wrap(err)
	}
	certReq := tlsca.CertificateRequest{
		Clock:     s.clock,
		PublicKey: csr.PublicKey,
		Subject:   csr.Subject,
		NotAfter:  s.clock.Now().UTC().Add(req.TTL.Get()),
	}
	// Include provided server name as a SAN in the certificate, CommonName
	// has been deprecated since Go 1.15:
	//   https://golang.org/doc/go1.15#commonname
	if req.ServerName != "" {
		certReq.DNSNames = []string{req.ServerName}
	}
	cert, err := tlsCA.GenerateCertificate(certReq)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	re := &proto.DatabaseCertResponse{Cert: cert}
	for _, ca := range hostCA.GetTLSKeyPairs() {
		re.CACerts = append(re.CACerts, ca.Cert)
	}
	return re, nil
}

// SignDatabaseCSR generates a client certificate used by proxy when talking
// to a remote database service.
func (s *Server) SignDatabaseCSR(ctx context.Context, req *proto.DatabaseCSRRequest) (*proto.DatabaseCSRResponse, error) {
	log.Debugf("Signing database CSR for cluster %v.", req.ClusterName)

	clusterName, err := s.GetClusterName()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	hostCA, err := s.GetCertAuthority(services.CertAuthID{
		Type:       services.HostCA,
		DomainName: req.ClusterName,
	}, false)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	csr, err := tlsca.ParseCertificateRequestPEM(req.CSR)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Extract the identity from the CSR, update "accepted usage" field to
	// indicate that the certificate can only be used for database proxy
	// server and re-encode the identity.
	id, err := tlsca.FromSubject(csr.Subject, time.Time{})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	id.Usage = []string{teleport.UsageDatabaseOnly}
	subject, err := id.Subject()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Extract user roles from the identity.
	roles, err := services.FetchRoles(id.Groups, s, id.Traits)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// Get the correct cert TTL based on roles.
	ttl := roles.AdjustSessionTTL(defaults.CertDuration)

	// Generate the TLS certificate.
	userCA, err := s.Trust.GetCertAuthority(services.CertAuthID{
		Type:       services.UserCA,
		DomainName: clusterName.GetClusterName(),
	}, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	tlsAuthority, err := userCA.TLSCA()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	tlsCert, err := tlsAuthority.GenerateCertificate(tlsca.CertificateRequest{
		Clock:     s.clock,
		PublicKey: csr.PublicKey,
		Subject:   subject,
		NotAfter:  s.clock.Now().UTC().Add(ttl),
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}

	re := &proto.DatabaseCSRResponse{Cert: tlsCert}
	for _, ca := range hostCA.GetTLSKeyPairs() {
		re.CACerts = append(re.CACerts, ca.Cert)
	}

	return re, nil
}
