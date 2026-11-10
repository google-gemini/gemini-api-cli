// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package substrate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	workerpb "github.com/google-gemini/gemini-api-cli/docs/self-hosted/agent-substrate/pkg/workerpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

const http2ClientPreface = "PRI * HTTP/2.0"

var blockedEnvKeys = map[string]bool{
	"PATH":                           true,
	"HOME":                           true,
	"LD_PRELOAD":                     true,
	"LD_LIBRARY_PATH":                true,
	"PYTHONPATH":                     true,
	"PYTHONHOME":                     true,
	"NODE_OPTIONS":                   true,
	"BASH_ENV":                       true,
	"ENV":                            true,
	"HTTP_PROXY":                     true,
	"HTTPS_PROXY":                    true,
	"NO_PROXY":                       true,
	"ALL_PROXY":                      true,
	"SSL_CERT_FILE":                  true,
	"SSL_CERT_DIR":                   true,
	"GOOGLE_APPLICATION_CREDENTIALS": true,
	"GEMINI_API_KEY":                 true,
	"GOOGLE_API_KEY":                 true,
	"GEMINI_ENVIRONMENT_KEY":         true,
	"ENVIRONMENT_KEY":                true,
	"GEMINI_ACCESS_TOKEN":            true,
	"ACCESS_TOKEN":                   true,
	"GOOGLE_OAUTH_ACCESS_TOKEN":      true,
	"CLOUDSDK_AUTH_ACCESS_TOKEN":     true,
	"GEMINI_LEASE_TOKEN":             true,
	"LEASE_TOKEN":                    true,
	"KUBERNETES_SERVICE_HOST":        true,
	"KUBERNETES_SERVICE_PORT":        true,
}

var scrubbedAmbientEnvKeys = map[string]bool{
	"GEMINI_API_KEY":                 true,
	"GOOGLE_API_KEY":                 true,
	"GEMINI_ENVIRONMENT_KEY":         true,
	"ENVIRONMENT_KEY":                true,
	"GEMINI_ACCESS_TOKEN":            true,
	"ACCESS_TOKEN":                   true,
	"GOOGLE_OAUTH_ACCESS_TOKEN":      true,
	"CLOUDSDK_AUTH_ACCESS_TOKEN":     true,
	"GOOGLE_APPLICATION_CREDENTIALS": true,
	"GEMINI_LEASE_TOKEN":             true,
	"LEASE_TOKEN":                    true,
	"KUBERNETES_SERVICE_HOST":        true,
	"KUBERNETES_SERVICE_PORT":        true,
}

func isBlockedEnvKey(k string) bool {
	upper := strings.ToUpper(k)
	if blockedEnvKeys[upper] {
		return true
	}
	return strings.HasPrefix(upper, "LD_") || strings.HasPrefix(upper, "KUBERNETES_SERVICE_")
}

func scrubDaemonBaseEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if scrubbedAmbientEnvKeys[upper] ||
			strings.HasPrefix(upper, "LD_") ||
			strings.HasPrefix(upper, "KUBERNETES_SERVICE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isValidEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		isLetter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
		isDigit := c >= '0' && c <= '9'
		if i == 0 && !isLetter {
			return false
		}
		if !isLetter && !isDigit {
			return false
		}
	}
	return true
}

// ActorDaemonConfig configures the gRPC server running inside a Substrate actor
// container (`ate-gemini actor-daemon`).
type ActorDaemonConfig struct {
	Port       int
	RuntimeBin string
	RootDir    string
	WorkDir    string
	ServerURL  string
	Logger     *slog.Logger
	Stderr     io.Writer
}

// Defaults populates default values on cfg.
func (cfg *ActorDaemonConfig) Defaults() {
	if cfg.Port <= 0 {
		cfg.Port = DefaultWorkerPort
	}
	if strings.TrimSpace(cfg.RuntimeBin) == "" {
		cfg.RuntimeBin = DefaultRuntimeBin
	}
	if strings.TrimSpace(cfg.RootDir) == "" {
		cfg.RootDir = "/"
	}
	if strings.TrimSpace(cfg.WorkDir) == "" {
		cfg.WorkDir = DefaultWorkDir
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		cfg.ServerURL = strings.TrimSpace(os.Getenv("GEMINI_API_BASE_URL"))
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
}

type actorWorkerServiceServer struct {
	workerpb.UnimplementedWorkerServiceServer
	cfg          ActorDaemonConfig
	daemonCtx    context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	shuttingDown bool
	inFlight     map[string]bool
	wg           sync.WaitGroup
}

func newActorWorkerServiceServer(ctx context.Context, cfg ActorDaemonConfig) *actorWorkerServiceServer {
	cfg.Defaults()
	if ctx == nil {
		ctx = context.Background()
	}
	daemonCtx, cancel := context.WithCancel(ctx)
	return &actorWorkerServiceServer{
		cfg:       cfg,
		daemonCtx: daemonCtx,
		cancel:    cancel,
		inFlight:  make(map[string]bool),
	}
}

func (s *actorWorkerServiceServer) shutdown() {
	s.mu.Lock()
	s.shuttingDown = true
	s.cancel()
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *actorWorkerServiceServer) GetIdleStatus(_ context.Context, _ *workerpb.GetIdleStatusRequest) (*workerpb.GetIdleStatusResponse, error) {
	s.mu.Lock()
	active := len(s.inFlight)
	s.mu.Unlock()
	return &workerpb.GetIdleStatusResponse{
		Running:            active > 0,
		ActiveInteractions: int32(active),
	}, nil
}

func (s *actorWorkerServiceServer) StartWork(_ context.Context, req *workerpb.StartWorkRequest) (*workerpb.StartWorkResponse, error) {
	intID := strings.TrimSpace(req.GetInteractionId())
	tok := strings.TrimSpace(req.GetLeaseToken())
	envID := strings.TrimSpace(req.GetEnvironmentId())
	if intID == "" || tok == "" {
		return nil, status.Error(codes.InvalidArgument, "interaction_id and lease_token are required")
	}
	if strings.ContainsAny(intID, "\x00\r\n") || strings.ContainsAny(tok, "\x00\r\n") || strings.ContainsAny(envID, "\x00\r\n") {
		return nil, status.Error(codes.InvalidArgument, "request identifiers must not contain control characters")
	}

	serverURL := strings.TrimSpace(req.GetControlPlaneUrl())
	if serverURL == "" {
		serverURL = s.cfg.ServerURL
	}
	if serverURL != "" {
		if err := validateControlPlaneURL(serverURL); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	cmdEnv := scrubDaemonBaseEnv(os.Environ())
	if ec := req.GetEnvironmentConfig(); ec != nil {
		for k, v := range ec.GetEnv() {
			if !isValidEnvKey(k) {
				return nil, status.Errorf(codes.InvalidArgument, "invalid environment variable name %q", k)
			}
			if isBlockedEnvKey(k) {
				return nil, status.Errorf(codes.InvalidArgument, "reserved environment variable %q cannot be overridden", k)
			}
			if strings.ContainsRune(v, '\x00') {
				return nil, status.Errorf(codes.InvalidArgument, "environment variable %q contains NUL byte", k)
			}
			cmdEnv = append(cmdEnv, fmt.Sprintf("%s=%s", k, v))
		}
	}

	s.mu.Lock()
	if s.shuttingDown || s.daemonCtx.Err() != nil {
		s.mu.Unlock()
		return nil, status.Error(codes.Unavailable, "actor daemon is shutting down")
	}
	if s.inFlight[intID] {
		s.mu.Unlock()
		return &workerpb.StartWorkResponse{
			Status: "already_running",
		}, nil
	}
	s.inFlight[intID] = true
	s.wg.Add(1)
	s.mu.Unlock()

	args := []string{
		"environments",
		"serve",
		"--root-dir=" + s.cfg.RootDir,
		"--workdir=" + s.cfg.WorkDir,
		"--interaction-id=" + intID,
		"--lease-token=" + tok,
	}
	if serverURL != "" {
		args = append(args, "--server-url="+serverURL)
	}

	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inFlight, intID)
			s.mu.Unlock()
		}()

		s.cfg.Logger.Info("Spawning gemini-api environments serve for interaction",
			slog.String("interaction_id", intID),
			slog.String("environment_id", envID),
		)
		cmd := exec.CommandContext(s.daemonCtx, s.cfg.RuntimeBin, args...)
		cmd.Stdout = s.cfg.Stderr
		cmd.Stderr = s.cfg.Stderr
		cmd.Env = cmdEnv
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return cmd.Process.Signal(syscall.SIGTERM)
		}
		cmd.WaitDelay = 5 * time.Second
		if err := cmd.Run(); err != nil {
			s.cfg.Logger.Error("gemini-api environments serve exited with error",
				slog.String("interaction_id", intID),
				slog.String("environment_id", envID),
				slog.String("error", err.Error()),
			)
			return
		}
		s.cfg.Logger.Info("Completed gemini-api environments serve for interaction",
			slog.String("interaction_id", intID),
			slog.String("environment_id", envID),
		)
	}()

	return &workerpb.StartWorkResponse{
		Status: "accepted",
	}, nil
}

// RunActorDaemon starts the actor gRPC server on cfg.Port and serves until ctx is canceled.
func RunActorDaemon(ctx context.Context, cfg ActorDaemonConfig) error {
	cfg.Defaults()

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("listening on port %d: %w", cfg.Port, err)
	}
	return ServeActorDaemon(ctx, ln, cfg)
}

type routerAuthInfo struct {
	credentials.CommonAuthInfo
}

func (routerAuthInfo) AuthType() string {
	return "atenet"
}

type routerCredentials struct{}

func newRouterCredentials() credentials.TransportCredentials {
	return &routerCredentials{}
}

func (c *routerCredentials) ClientHandshake(_ context.Context, _ string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, routerAuthInfo{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}, nil
}

func (c *routerCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return conn, routerAuthInfo{CommonAuthInfo: credentials.CommonAuthInfo{SecurityLevel: credentials.NoSecurity}}, nil
}

func (c *routerCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "atenet"}
}

func (c *routerCredentials) Clone() credentials.TransportCredentials {
	return &routerCredentials{}
}

func (c *routerCredentials) OverrideServerName(string) error {
	return nil
}

// ServeActorDaemon serves WorkerService over gRPC (plus HTTP/1.1 GET /healthz
// for Substrate's WakeupProbe) on ln until ctx is canceled.
func ServeActorDaemon(ctx context.Context, ln net.Listener, cfg ActorDaemonConfig) error {
	cfg.Defaults()

	grpcServer := grpc.NewServer(grpc.Creds(newRouterCredentials()))
	workerSrv := newActorWorkerServiceServer(ctx, cfg)
	workerpb.RegisterWorkerServiceServer(grpcServer, workerSrv)

	healthSrv := health.NewServer()
	healthpb.RegisterHealthServer(grpcServer, healthSrv)
	healthSrv.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	muxLis := newMultiplexedListener(ln, healthMux)

	errCh := make(chan error, 1)
	go func() {
		cfg.Logger.Info("ate-gemini actor-daemon listening (gRPC WorkerService + /healthz)",
			slog.String("addr", ln.Addr().String()),
			slog.String("runtime_bin", cfg.RuntimeBin),
			slog.String("workdir", cfg.WorkDir),
		)
		if err := grpcServer.Serve(muxLis); err != nil {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		grpcServer.Stop()
		_ = muxLis.Close()
		workerSrv.shutdown()
		return nil
	case err := <-errCh:
		_ = muxLis.Close()
		workerSrv.shutdown()
		return err
	}
}

type peekedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *peekedConn) Read(p []byte) (int, error) {
	return c.br.Read(p)
}

type chanListener struct {
	addr   net.Addr
	connCh chan net.Conn
	doneCh chan struct{}
	once   sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		addr:   addr,
		connCh: make(chan net.Conn, 64),
		doneCh: make(chan struct{}),
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case conn, ok := <-l.connCh:
		if !ok {
			return nil, net.ErrClosed
		}
		return conn, nil
	case <-l.doneCh:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() {
		close(l.doneCh)
	})
	return nil
}

func (l *chanListener) Addr() net.Addr {
	return l.addr
}

type multiplexedListener struct {
	rawLis     net.Listener
	grpcLis    *chanListener
	httpLis    *chanListener
	httpServer *http.Server
	closeOnce  sync.Once
}

func newMultiplexedListener(rawLis net.Listener, httpHandler http.Handler) *multiplexedListener {
	addr := rawLis.Addr()
	m := &multiplexedListener{
		rawLis:  rawLis,
		grpcLis: newChanListener(addr),
		httpLis: newChanListener(addr),
		httpServer: &http.Server{
			Handler:           httpHandler,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
	go func() {
		_ = m.httpServer.Serve(m.httpLis)
	}()
	go m.acceptLoop()
	return m
}

func (m *multiplexedListener) acceptLoop() {
	for {
		conn, err := m.rawLis.Accept()
		if err != nil {
			_ = m.grpcLis.Close()
			_ = m.httpLis.Close()
			return
		}
		go m.routeConn(conn)
	}
}

func (m *multiplexedListener) routeConn(conn net.Conn) {
	br := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	prefix, err := br.Peek(len(http2ClientPreface))
	_ = conn.SetReadDeadline(time.Time{})

	pConn := &peekedConn{Conn: conn, br: br}
	if err == nil && string(prefix) != http2ClientPreface {
		select {
		case m.httpLis.connCh <- pConn:
		case <-m.httpLis.doneCh:
			_ = pConn.Close()
		}
		return
	}

	select {
	case m.grpcLis.connCh <- pConn:
	case <-m.grpcLis.doneCh:
		_ = pConn.Close()
	}
}

func (m *multiplexedListener) Accept() (net.Conn, error) {
	return m.grpcLis.Accept()
}

func (m *multiplexedListener) Close() error {
	var err error
	m.closeOnce.Do(func() {
		err = m.rawLis.Close()
		_ = m.grpcLis.Close()
		_ = m.httpLis.Close()
		_ = m.httpServer.Close()
	})
	return err
}

func (m *multiplexedListener) Addr() net.Addr {
	return m.rawLis.Addr()
}
