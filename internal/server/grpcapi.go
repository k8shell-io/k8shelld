// grpcapi.go, copyright 2025 the k8shell.io authors

// gRPC API service creates a gRPC server, registers the services, sets up the TLS configuration,
// and the interceptor. It uses the tokenAuthInterceptor to authenticate the client using the token
// in the metadata.

package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const cleanupInterval = 1 * time.Minute // The interval for cleaning up the stores
const deleteDelay = 1 * time.Minute     // The delay after the channel was stopped before deleting an entry
const timeFormat = time.RFC3339         // The time format for the created and deleted fields

type StoreRecord struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Created  string `json:"created"`
	Deleted  string `json:"deleted"`
	Status   string `json:"status"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
	Params   string `json:"params"`
}

// GRPCApiService is the main service that handles the gRPC API
type GRPCApiService struct {
	logger             *zerolog.Logger      // The logger
	initScriptsDir     string               // The directory where the init scripts are located
	accessToken        string               // The access token that a client needs to auhtenticate with
	tcpPort            int                  // The TCP port that the gRPC server listens on
	cert               tls.Certificate      // The TLS certificate and key pair
	KeyLogFilePath     string               // The path to the key log file for debugging
	user               User                 // The main workspace user
	portForwadingRules []PortForwardingRule // The port forwarding rules that are allowed
	execStore          *sync.Map            // The store for the exec data
	portForwardStore   *sync.Map            // The store for the port forwarding data
	sessionStore       *sync.Map            // The store for the session data
	unixSocketStore    *sync.Map            // The store for the unix socket data
}

// RemoteOSServiceServer is the service that handles the remote OS GRPC service server
type RemoteOSServiceServer struct {
	grpcApi *GRPCApiService
	k8shelldpb.UnimplementedRemoteOSServiceServer
}

// InfoServiceServer is the service that handles the info GRPC service server
type InfoServiceServer struct {
	grpcApi *GRPCApiService
	k8shelldpb.UnimplementedInfoServiceServer
}

// Helper function to get the deletion date as a string or empty if not set
func getDeletedDate(deleted time.Time) string {
	if deleted.IsZero() {
		return ""
	}
	return deleted.Format(timeFormat)
}

// Helper function to determine the status
func getStatus(deleted time.Time) string {
	if deleted.IsZero() {
		return "ACTIVE"
	}
	return "STOPPED"
}

// DecryptAES decrypts AES-GCM encrypted data using the provided key.
func DecryptAES(accessKey string, encryptedData []byte) ([]byte, error) {
	encryptedStr := string(encryptedData)

	const prefix = "ENC[AES256]"
	if !strings.HasPrefix(encryptedStr, prefix) {
		//return nil, errors.New("invalid encryption format: missing ENC[AES256] prefix")
		return []byte(encryptedStr), nil
	}
	encryptedStr = strings.TrimPrefix(encryptedStr, prefix)

	dataBytes, err := base64.StdEncoding.DecodeString(encryptedStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 data: %v", err)
	}

	keyBytes, err := hex.DecodeString(accessKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode hex key: %v", err)
	}

	if len(keyBytes) != 16 && len(keyBytes) != 24 && len(keyBytes) != 32 {
		return nil, fmt.Errorf("invalid AES key length: must be 16, 24, or 32 bytes")
	}

	if len(dataBytes) < 28 {
		return nil, fmt.Errorf("invalid encrypted data length")
	}

	nonce := dataBytes[:12]      // First 12 bytes = nonce
	tag := dataBytes[12:28]      // Next 16 bytes = authentication tag
	ciphertext := dataBytes[28:] // Remaining bytes = encrypted content

	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %v", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM mode: %v", err)
	}

	plaintext, err := aesGCM.Open(nil, nonce, append(ciphertext, tag...), nil)
	if err != nil {
		return nil, fmt.Errorf("decryption failed: %v", err)
	}

	return plaintext, nil
}

// LoadDecryptedKeyPair loads a TLS certificate and key pair from files.
func LoadDecryptedKeyPair(serverCertPath, encryptedKeyPath, accessKey string) (tls.Certificate, error) {
	encryptedKey, err := os.ReadFile(encryptedKeyPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to read encrypted key file: %v", err)
	}

	decryptedKey, err := DecryptAES(accessKey, encryptedKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to decrypt key: %v", err)
	}

	certPEM, err := os.ReadFile(serverCertPath)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to read certificate file: %v", err)
	}

	cert, err := tls.X509KeyPair(certPEM, decryptedKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to load decrypted certificate and key: %v", err)
	}

	return cert, nil
}

// NewGRPCAPI creates a new GRPCApiService
func NewGRPCAPI(tcpPort int, accessKey string, user User,
	serverKeyPath string, serverCertPath string, keyLogFilePath string,
	portForwardingRules []PortForwardingRule, initScriptsDir string) (*GRPCApiService, error) {

	logger := log.NewLogger("grpc")

	cert, err := LoadDecryptedKeyPair(serverCertPath, serverKeyPath, accessKey)
	if err != nil {
		logger.Fatal().Msgf("Failed to load certificate and key: %v", err)
	}

	return &GRPCApiService{
		logger:             logger,
		initScriptsDir:     initScriptsDir,
		accessToken:        accessKey,
		KeyLogFilePath:     keyLogFilePath,
		tcpPort:            tcpPort,
		cert:               cert,
		user:               user,
		portForwadingRules: portForwardingRules,
		execStore:          &sync.Map{},
		portForwardStore:   &sync.Map{},
		sessionStore:       &sync.Map{},
		unixSocketStore:    &sync.Map{},
	}, nil
}

// tokenAuthInterceptor is a gRPC interceptor that checks the token in the metadata.
// It is used to authenticate the client.
func (a *GRPCApiService) tokenAuthInterceptor(ctx context.Context) (context.Context, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing metadata")
	}

	tokens := md["authorization"]
	if len(tokens) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing token")
	}
	token := tokens[0]

	if token != a.accessToken {
		return nil, status.Error(codes.PermissionDenied, "invalid token")
	}

	return ctx, nil
}

// unaryAuthInterceptor returns a gRPC interceptor that performs token-based authentication.
func (a *GRPCApiService) unaryAuthInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		ctx, err := a.tokenAuthInterceptor(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// UnaryErrorLoggingInterceptor logs all gRPC errors returned by handlers.
func (a *GRPCApiService) unaryErrorLoggingInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		resp, err = handler(ctx, req)
		if err != nil {
			st := status.Convert(err)
			a.logger.Error().
				Str("method", info.FullMethod).
				Str("code", st.Code().String()).
				Err(err).
				Msg("gRPC error occurred")
		}
		return resp, err
	}
}

// Handle starts the gRPC server and registers the services.
// It also sets up the TLS configuration and the interceptor.
func (a *GRPCApiService) Handler(ctx context.Context) error {
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{a.cert},
	}
	if a.KeyLogFilePath != "" {
		file, err := os.OpenFile(a.KeyLogFilePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
		if err != nil {
			a.logger.Warn().Msgf("Failed to open key log file: %v", err)
		} else {
			defer file.Close()
			tlsConfig.KeyLogWriter = file
		}
	}

	creds := credentials.NewTLS(tlsConfig)
	server := grpc.NewServer(grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(
			a.unaryAuthInterceptor(),
			a.unaryErrorLoggingInterceptor(),
		),
	)

	k8shelldpb.RegisterInfoServiceServer(server, NewInfoServiceServer(a))
	k8shelldpb.RegisterInitServiceServer(server, NewInitServiceServer(a))
	k8shelldpb.RegisterRemoteOSServiceServer(server, NewRemoteOSServiceServer(a))

	a.logger.Info().Msgf("GRPC services server registered")

	listener, err := net.Listen("tcp4", fmt.Sprintf(":%d", a.tcpPort))
	if err != nil {
		return fmt.Errorf("failed to listen: %v", err)
	}

	// Start periodic cleanup goroutine
	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				a.cleanupChannelStores()
			case <-ctx.Done():
				a.logger.Info().Msgf("Cleanup goroutine exiting")
				return
			}
		}
	}()

	// Start gRPC server in background
	errChan := make(chan error, 1)
	go func() {
		a.logger.Info().Msgf("GRPC service listening on :%d", a.tcpPort)
		if err := server.Serve(listener); err != nil && err != grpc.ErrServerStopped {
			errChan <- fmt.Errorf("gRPC server error: %v", err)
		}
	}()

	// Wait for context cancel or server error
	select {
	case <-ctx.Done():
		a.logger.Info().Msg("Shutting down gRPC server")
		server.GracefulStop()
		return nil
	case err := <-errChan:
		return err
	}
}

// NewRemoteOSServiceServer creates a new RemoteOSServiceServer
func NewRemoteOSServiceServer(grpcapi *GRPCApiService) *RemoteOSServiceServer {
	return &RemoteOSServiceServer{
		grpcApi: grpcapi,
	}
}

// NewRemoteOSServiceServer creates a new RemoteOSServiceServer
func NewInfoServiceServer(grpcapi *GRPCApiService) *InfoServiceServer {
	return &InfoServiceServer{
		grpcApi: grpcapi,
	}
}

func (s *InfoServiceServer) Version(ctx context.Context, req *k8shelldpb.VersionRequest) (*k8shelldpb.VersionResponse, error) {
	return &k8shelldpb.VersionResponse{
		Version: K8SHELLD_VERSION,
		Commit:  K8SHELLD_COMMIT,
	}, nil
}

// Cleanup the stores by removing the entries that were deleted more than deleteDelay ago
func (a *GRPCApiService) cleanupChannelStores() {
	a.cleanupChannelStore(a.execStore, func(v any) bool {
		data := v.(*ExecData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.portForwardStore, func(v any) bool {
		data := v.(*PortForwardData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.sessionStore, func(v any) bool {
		data := v.(*SessionData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.unixSocketStore, func(v any) bool {
		data := v.(*unixSocketData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

}

// Generic cleanup function for any store
func (a *GRPCApiService) cleanupChannelStore(store *sync.Map, shouldDelete func(any) bool) {
	store.Range(func(key, value any) bool {
		if shouldDelete(value) {
			store.Delete(key)
		}
		return true
	})
}

func (a *GRPCApiService) getAllChannelStoreData() ([]StoreRecord, error) {
	var result []StoreRecord

	// Helper function to process each store
	processStore := func(storeName string, store *sync.Map) {
		store.Range(func(key, value any) bool {
			switch v := value.(type) {
			case *ExecData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("cmd=%s", v.Command),
				}
				result = append(result, record)
			case *PortForwardData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("dest=%s, port=%d", v.Destination, v.Port),
				}
				result = append(result, record)
			case *SessionData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("cmd=%s, pid=%d", v.CmdShell, v.Pid),
				}
				result = append(result, record)
			case *unixSocketData:
				record := StoreRecord{
					Id:       v.Id,
					Name:     storeName,
					Created:  v.Created.Format(timeFormat),
					Deleted:  getDeletedDate(v.Deleted),
					Status:   getStatus(v.Deleted),
					BytesIn:  v.BytesIn,
					BytesOut: v.BytesOut,
					Params:   fmt.Sprintf("socket=%s", v.socketPath),
				}
				result = append(result, record)
			}
			return true
		})
	}

	processStore("exec", a.execStore)
	processStore("port-forward", a.portForwardStore)
	processStore("shell", a.sessionStore)
	processStore("unix-socket", a.unixSocketStore)

	return result, nil
}
