// grpcapi.go, copyright 2025 the k8shell.io authors

// gRPC API service creates a gRPC server, registers the services, sets up the TLS configuration,
// and the interceptor. It uses the tokenAuthInterceptor to authenticate the client using the token
// in the metadata.

package grpc

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/k8shell-io/common/pkg/gapi"
	"github.com/k8shell-io/k8shelld/internal/config"
	"github.com/k8shell-io/k8shelld/internal/log"
	"github.com/k8shell-io/k8shelld/internal/system"
	"github.com/k8shell-io/k8shelld/pkg/api/k8shelldpb"

	apiClient "github.com/k8shell-io/api-server/pkg/client"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
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
type GRPCService struct {
	grpcConfig          gapi.ServerConfig           // The gRPC server configuration
	logger              *zerolog.Logger             // The logger
	initScriptsDir      string                      // The directory where the init scripts are located
	user                system.User                 // The workspace owner
	procWatcher         *system.ProcessWatcher      // The process watcher
	portForwardingRules []config.PortForwardingRule // The port forwarding rules that are allowed
	ExecStore           *sync.Map                   // The store for the exec data
	PortForwardStore    *sync.Map                   // The store for the port forwarding data
	SessionStore        *sync.Map                   // The store for the session data
	UnixSocketStore     *sync.Map                   // The store for the unix socket data
	apiClient           *apiClient.Client           // The API client to communicate with the API server
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
func NewGRPCService(user system.User, grpcConfig gapi.ServerConfig,
	portForwardingRules []config.PortForwardingRule, initScriptsDir string,
	procWatcher *system.ProcessWatcher, apiClient *apiClient.Client) (*GRPCService, error) {

	logger := log.NewLogger("grpc")

	return &GRPCService{
		logger:              logger,
		initScriptsDir:      initScriptsDir,
		grpcConfig:          grpcConfig,
		user:                user,
		portForwardingRules: portForwardingRules,
		procWatcher:         procWatcher,
		ExecStore:           &sync.Map{},
		PortForwardStore:    &sync.Map{},
		SessionStore:        &sync.Map{},
		UnixSocketStore:     &sync.Map{},
		apiClient:           apiClient,
	}, nil
}

// Serve starts the gRPC server and registers the services.
// It also sets up the TLS configuration and the interceptor.
func (a *GRPCService) Serve(ctx context.Context) error {

	server, err := gapi.NewServer(&a.grpcConfig)
	if err != nil {
		return fmt.Errorf("failed to create gRPC server: %v", err)
	}

	k8shelldpb.RegisterSystemServiceServer(server.GrpcServer, NewSystemServiceServer(a))
	k8shelldpb.RegisterShellServiceServer(server.GrpcServer, NewShellServiceServer(a))
	k8shelldpb.RegisterExecServiceServer(server.GrpcServer, NewExecServiceServer(a))
	k8shelldpb.RegisterPortForwardServiceServer(server.GrpcServer, NewPortForwardServiceServer(a))
	k8shelldpb.RegisterUnixSocketServiceServer(server.GrpcServer, NewUnixSocketServiceServer(a))

	a.logger.Info().Msgf("GRPC services server registered")

	// cleanup goroutine
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

	errChan := make(chan error, 1)
	go func() {
		if err := server.Start(); err != nil && err != grpc.ErrServerStopped {
			errChan <- fmt.Errorf("gRPC server error: %v", err)
		}
	}()

	select {
	case <-ctx.Done():
		a.logger.Info().Msg("Shutting down gRPC server")
		server.Stop()
		return nil
	case err := <-errChan:
		return err
	}
}

// Cleanup the stores by removing the entries that were deleted more than deleteDelay ago
func (a *GRPCService) cleanupChannelStores() {
	a.cleanupChannelStore(a.ExecStore, func(v any) bool {
		data := v.(*ExecData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.PortForwardStore, func(v any) bool {
		data := v.(*PortForwardData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.SessionStore, func(v any) bool {
		data := v.(*SessionData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

	a.cleanupChannelStore(a.UnixSocketStore, func(v any) bool {
		data := v.(*unixSocketData)
		return !data.Deleted.IsZero() && time.Since(data.Deleted) > deleteDelay
	})

}

// Generic cleanup function for any store
func (a *GRPCService) cleanupChannelStore(store *sync.Map, shouldDelete func(any) bool) {
	store.Range(func(key, value any) bool {
		if shouldDelete(value) {
			store.Delete(key)
		}
		return true
	})
}

func (a *GRPCService) GetAllChannelStoreData() ([]StoreRecord, error) {
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

	processStore("exec", a.ExecStore)
	processStore("port-forward", a.PortForwardStore)
	processStore("shell", a.SessionStore)
	processStore("unix-socket", a.UnixSocketStore)

	return result, nil
}
