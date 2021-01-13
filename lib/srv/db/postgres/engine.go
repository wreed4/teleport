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

package postgres

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"

	"github.com/gravitational/teleport/api/client/proto"
	"github.com/gravitational/teleport/lib/auth"
	"github.com/gravitational/teleport/lib/auth/native"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/srv/db/session"
	"github.com/gravitational/teleport/lib/tlsca"
	"github.com/gravitational/teleport/lib/utils"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/service/rds/rdsutils"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgproto3/v2"

	"github.com/gravitational/trace"
	"github.com/jonboulle/clockwork"
	"github.com/sirupsen/logrus"
)

// Engine implements the Postgres database service that accepts client
// connections coming over reverse tunnel from the proxy and proxies
// them between the proxy and the Postgres database instance.
//
// Implements db.DatabaseEngine.
type Engine struct {
	// AuthClient is the cluster auth client.
	AuthClient *auth.Client
	// Credentials are the AWS credentials used to generate RDS auth tokens.
	Credentials *credentials.Credentials
	// RDSCACerts contains AWS RDS root certificates.
	RDSCACerts map[string][]byte
	// StreamWriter is the async audit logger.
	StreamWriter events.StreamWriter
	// OnSessionStart is called upon successful connection to the database.
	OnSessionStart func(session.Context, error) error
	// OnSessionEnd is called upon disconnection from the database.
	OnSessionEnd func(session.Context) error
	// OnQuery is called when an SQL query is executed on the connection.
	OnQuery func(session.Context, string) error
	// Clock is the clock interface.
	Clock clockwork.Clock
	// Log is used for logging.
	Log logrus.FieldLogger
}

// toErrorResponse converts the provided error to a Postgres wire protocol
// error message response so the client such as psql can display it
// appropriately.
func toErrorResponse(err error) *pgproto3.ErrorResponse {
	var pgErr *pgconn.PgError
	if !errors.As(trace.Unwrap(err), &pgErr) {
		return &pgproto3.ErrorResponse{
			Message: err.Error(),
		}
	}
	return &pgproto3.ErrorResponse{
		Severity: pgErr.Severity,
		Code:     pgErr.Code,
		Message:  pgErr.Message,
		Detail:   pgErr.Detail,
	}
}

// HandleConnection processes the connection from Postgres proxy coming
// over reverse tunnel.
//
// It handles all necessary startup actions, authorization and acts as a
// middleman between the proxy and the database intercepting and interpreting
// all messages i.e. doing protocol parsing.
func (e *Engine) HandleConnection(ctx context.Context, sessionCtx *session.Context, clientConn net.Conn) (err error) {
	client := pgproto3.NewBackend(pgproto3.NewChunkReader(clientConn), clientConn)
	defer func() {
		if err != nil {
			if err := client.Send(toErrorResponse(err)); err != nil {
				e.Log.WithError(err).Error("Failed to send error to client.")
			}
		}
	}()
	// The proxy is supposed to pass a startup message it received from
	// the psql client over to us, so wait for it and extract database
	// and username from it.
	err = e.handleStartup(client, sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	// Now we know which database/username the user is connecting to, so
	// perform an authorization check.
	err = e.checkAccess(sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	// This is where we connect to the actual Postgres database.
	server, hijackedConn, err := e.connect(ctx, sessionCtx)
	if err != nil {
		return trace.Wrap(err)
	}
	// Upon successful connect, indicate to the Postgres client that startup
	// has been completed and it can start sending queries.
	err = e.makeClientReady(client, hijackedConn)
	if err != nil {
		return trace.Wrap(err)
	}
	// At this point Postgres client should be ready to start sending
	// messages: this is where psql prompt appears on the other side.
	err = e.OnSessionStart(*sessionCtx, nil)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		err := e.OnSessionEnd(*sessionCtx)
		if err != nil {
			e.Log.WithError(err).Error("Failed to emit audit event.")
		}
	}()
	// Reconstruct pgconn.PgConn from hijacked connection for easier access
	// to its utility methods (such as Close).
	serverConn, err := pgconn.Construct(hijackedConn)
	if err != nil {
		return trace.Wrap(err)
	}
	defer func() {
		err = serverConn.Close(ctx)
		if err != nil {
			e.Log.WithError(err).Error("Failed to close connection.")
		}
	}()
	// Now launch the message exchange relaying all intercepted messages b/w
	// the client (psql or other Postgres client) and the server (database).
	clientErrCh := make(chan error, 1)
	serverErrCh := make(chan error, 1)
	go e.receiveFromClient(client, server, clientErrCh, sessionCtx)
	go e.receiveFromServer(server, client, serverConn, serverErrCh, sessionCtx)
	select {
	case err := <-clientErrCh:
		e.Log.WithError(err).Debug("Client done.")
	case err := <-serverErrCh:
		e.Log.WithError(err).Debug("Server done.")
	case <-ctx.Done():
		e.Log.Debug("Context canceled.")
	}
	return nil
}

// handleStartup receives a startup message from the proxy and updates
// the session context with the connection parameters.
func (e *Engine) handleStartup(client *pgproto3.Backend, sessionCtx *session.Context) error {
	startupMessageI, err := client.ReceiveStartupMessage()
	if err != nil {
		return trace.Wrap(err)
	}
	e.Log.Debugf("Received startup message: %#v.", startupMessageI)
	startupMessage, ok := startupMessageI.(*pgproto3.StartupMessage)
	if !ok {
		return trace.BadParameter("expected *pgproto3.StartupMessage, got %T", startupMessageI)
	}
	// Pass startup parameters received from the client along (this is how the
	// client sets default date style format for example), but remove database
	// name and user from them.
	for key, value := range startupMessage.Parameters {
		switch key {
		case "database":
			sessionCtx.DatabaseName = value
		case "user":
			sessionCtx.DatabaseUser = value
		default:
			sessionCtx.StartupParameters[key] = value
		}
	}
	return nil
}

func (e *Engine) checkAccess(sessionCtx *session.Context) error {
	err := sessionCtx.Checker.CheckAccessToDatabase(sessionCtx.Server,
		sessionCtx.DatabaseName, sessionCtx.DatabaseUser)
	if err != nil {
		if err := e.OnSessionStart(*sessionCtx, err); err != nil {
			e.Log.WithError(err).Error("Failed to emit audit event.")
		}
		return trace.Wrap(err)
	}
	return nil
}

// connect establishes the connection to the database instance and returns
// the hijacked connection and the frontend, an interface used for message
// exchange with the database.
func (e *Engine) connect(ctx context.Context, sessionCtx *session.Context) (*pgproto3.Frontend, *pgconn.HijackedConn, error) {
	connectConfig, err := e.getConnectConfig(ctx, sessionCtx)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	// TODO(r0mant): Instead of using pgconn to connect, in future it might
	// make sense to reimplement the connect logic which will give us more
	// control over the initial startup and ability to relay authentication
	// messages b/w server and client e.g. to get client's password.
	conn, err := pgconn.ConnectConfig(ctx, connectConfig)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	// Hijacked connection exposes some internal connection data, such as
	// parameters we'll need to relay back to the client (e.g. database
	// server version).
	hijackedConn, err := conn.Hijack()
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	frontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(hijackedConn.Conn), hijackedConn.Conn)
	return frontend, hijackedConn, nil
}

// makeClientReady indicates to the Postgres client (such as psql) that the
// server is ready to accept messages from it.
func (e *Engine) makeClientReady(client *pgproto3.Backend, hijackedConn *pgconn.HijackedConn) error {
	// AuthenticationOk indicates that the authentication was successful.
	e.Log.Debug("Sending AuthenticationOk.")
	if err := client.Send(&pgproto3.AuthenticationOk{}); err != nil {
		return trace.Wrap(err)
	}
	// BackendKeyData provides secret-key data that the frontend must save
	// if it wants to be able to issue cancel requests later.
	e.Log.Debugf("Sending BackendKeyData: PID=%v.", hijackedConn.PID)
	if err := client.Send(&pgproto3.BackendKeyData{ProcessID: hijackedConn.PID, SecretKey: hijackedConn.SecretKey}); err != nil {
		return trace.Wrap(err)
	}
	// ParameterStatuses contains parameters reported by the server such as
	// server version, relay them back to the client.
	e.Log.Debugf("Sending ParameterStatuses: %v.", hijackedConn.ParameterStatuses)
	for k, v := range hijackedConn.ParameterStatuses {
		if err := client.Send(&pgproto3.ParameterStatus{Name: k, Value: v}); err != nil {
			return trace.Wrap(err)
		}
	}
	// ReadyForQuery indicates that the start-up is completed and the
	// frontend can now issue commands.
	e.Log.Debug("Sending ReadyForQuery")
	if err := client.Send(&pgproto3.ReadyForQuery{}); err != nil {
		return trace.Wrap(err)
	}
	return nil
}

// receiveFromClient receives messages from the provided backend (which
// in turn receives them from psql or other client) and relays them to
// the frontend connected to the database instance.
func (e *Engine) receiveFromClient(client *pgproto3.Backend, server *pgproto3.Frontend, clientErrCh chan<- error, sessionCtx *session.Context) {
	log := e.Log.WithField("from", "client")
	defer log.Debug("Stop receiving from client.")
	for {
		message, err := client.Receive()
		if err != nil {
			log.WithError(err).Errorf("Failed to receive message from client.")
			clientErrCh <- err
			return
		}
		log.Debugf("Received client message: %#v.", message)
		switch msg := message.(type) {
		case *pgproto3.Query:
			err := e.OnQuery(*sessionCtx, msg.String)
			if err != nil {
				log.WithError(err).Error("Failed to emit audit event.")
			}
		case *pgproto3.Terminate:
			clientErrCh <- nil
			return
		}
		err = server.Send(message)
		if err != nil {
			log.WithError(err).Error("Failed to send message to server.")
			clientErrCh <- err
			return
		}
	}
}

// receiveFromServer receives messages from the provided frontend (which
// is connected to the database instance) and relays them back to the psql
// or other client via the provided backend.
func (e *Engine) receiveFromServer(server *pgproto3.Frontend, client *pgproto3.Backend, serverConn *pgconn.PgConn, serverErrCh chan<- error, sessionCtx *session.Context) {
	log := e.Log.WithField("from", "server")
	defer log.Debug("Stop receiving from server.")
	for {
		message, err := server.Receive()
		if err != nil {
			if serverConn.IsClosed() {
				log.Debug("Server connection closed.")
				serverErrCh <- nil
				return
			}
			log.WithError(err).Errorf("Failed to receive message from server.")
			serverErrCh <- err
			return
		}
		log.Debugf("Received server message: %#v.", message)
		// This is where we would plug in custom logic for particular
		// messages received from the Postgres server (i.e. emitting
		// an audit event), but for now just pass them along back to
		// the client.
		err = client.Send(message)
		if err != nil {
			log.WithError(err).Error("Failed to send message to client.")
			serverErrCh <- err
			return
		}
	}
}

// getConnectConfig returns config that can be used to connect to the
// database instance.
func (e *Engine) getConnectConfig(ctx context.Context, sessionCtx *session.Context) (*pgconn.Config, error) {
	// The driver requires the config to be built by parsing the connection
	// string so parse the basic template and then fill in the rest of
	// parameters such as TLS configuration.
	config, err := pgconn.ParseConfig(fmt.Sprintf("postgres://%s@%s/?database=%s",
		sessionCtx.DatabaseUser, sessionCtx.Server.GetURI(), sessionCtx.DatabaseName))
	if err != nil {
		return nil, trace.Wrap(err)
	}
	// Pgconn adds fallbacks to retry connection without TLS if the TLS
	// attempt fails. Reset the fallbacks to avoid retries, otherwise
	// it's impossible to debug TLS connection errors.
	config.Fallbacks = nil
	// Set startup parameters that the client sent us.
	config.RuntimeParams = sessionCtx.StartupParameters
	// RDS/Aurora use IAM authentication so request an auth token and
	// use it as a password.
	if sessionCtx.Server.IsRDS() {
		config.Password, err = e.getRDSAuthToken(sessionCtx)
		if err != nil {
			return nil, trace.Wrap(err)
		}
	}
	// TLS config will use client certificate for an onprem database or
	// will contain RDS root certificate for RDS/Aurora.
	config.TLSConfig, err = e.getTLSConfig(ctx, sessionCtx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	return config, nil
}

// getRDSAuthToken returns authorization token that will be used as a password
// when connecting to RDS/Aurora databases.
func (e *Engine) getRDSAuthToken(sessionCtx *session.Context) (string, error) {
	e.Log.Debugf("Generating auth token for %s.", sessionCtx)
	return rdsutils.BuildAuthToken(
		sessionCtx.Server.GetURI(),
		sessionCtx.Server.GetRegion(),
		sessionCtx.DatabaseUser,
		e.Credentials)
}

// getTLSConfig builds the client TLS configuration for the session.
//
// For RDS/Aurora, the config must contain RDS root certificate as a trusted
// authority. For onprem we generate a client certificate signed by the host
// CA used to authenticate.
func (e *Engine) getTLSConfig(ctx context.Context, sessionCtx *session.Context) (*tls.Config, error) {
	addr, err := utils.ParseAddr(sessionCtx.Server.GetURI())
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsConfig := &tls.Config{
		ServerName: addr.Host(),
		RootCAs:    x509.NewCertPool(),
	}
	// Add CA certificate to the trusted pool if it's present, e.g. when
	// connecting to RDS/Aurora which require AWS CA.
	if len(sessionCtx.Server.GetCA()) != 0 {
		if !tlsConfig.RootCAs.AppendCertsFromPEM(sessionCtx.Server.GetCA()) {
			return nil, trace.BadParameter("failed to append CA certificate to the pool")
		}
	} else if sessionCtx.Server.IsRDS() {
		if rdsCA, ok := e.RDSCACerts[sessionCtx.Server.GetRegion()]; ok {
			if !tlsConfig.RootCAs.AppendCertsFromPEM(rdsCA) {
				return nil, trace.BadParameter("failed to append CA certificate to the pool")
			}
		} else {
			e.Log.Warnf("No RDS CA certificate for %v.", sessionCtx.Server)
		}
	}
	// RDS/Aurora auth is done via an auth token so don't generate a client
	// certificate and exit here.
	if sessionCtx.Server.IsRDS() {
		return tlsConfig, nil
	}
	// Otherwise, when connecting to an onprem database, generate a client
	// certificate. The database instance should be configured with
	// Teleport's CA obtained with 'tctl auth sign --type=db'.
	cert, cas, err := e.getClientCert(ctx, sessionCtx)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	tlsConfig.Certificates = []tls.Certificate{*cert}
	for _, ca := range cas {
		if !tlsConfig.RootCAs.AppendCertsFromPEM(ca) {
			return nil, trace.BadParameter("failed to append CA certificate to the pool")
		}
	}
	return tlsConfig, nil
}

// getClientCert signs an ephemeral client certificate used by this
// server to authenticate with the database instance.
func (e *Engine) getClientCert(ctx context.Context, sessionCtx *session.Context) (cert *tls.Certificate, cas [][]byte, err error) {
	privateBytes, _, err := native.GenerateKeyPair("")
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	// Postgres requires the database username to be encoded as a common
	// name in the client certificate.
	subject := pkix.Name{CommonName: sessionCtx.DatabaseUser}
	csr, err := tlsca.GenerateCertificateRequestPEM(subject, privateBytes)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	// TODO(r0mant): Cache database certificates to avoid expensive generate
	// operation on each connection.
	e.Log.Debugf("Generating client certificate for %s.", sessionCtx)
	resp, err := e.AuthClient.GenerateDatabaseCert(ctx, &proto.DatabaseCertRequest{
		CSR: csr,
		TTL: proto.Duration(sessionCtx.Identity.Expires.Sub(e.Clock.Now())),
	})
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	clientCert, err := tls.X509KeyPair(resp.Cert, privateBytes)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	return &clientCert, resp.CACerts, nil
}
