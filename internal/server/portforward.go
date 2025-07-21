// portforward.go, copyright 2025 the k8shell.io authors

// Port-forwarding service server. It creates a new port-forwarding instance, starts the TCP connection, and
// streams data between the client and the destination. The port forwarder is able to forward the traffic to
// the destination only if the destination IP is in the allowed subnets. The allowed subnets are defined in the
// port-forwarding rules.

package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/k8shell-io/k8shelld/grpc/generated-go/k8shelldpb"
	"github.com/k8shell-io/k8shelld/internal/log"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Port-forward data structure
type PortForwardData struct {
	Id          string
	Destination string
	Port        uint16
	Created     time.Time
	Deleted     time.Time
	BytesIn     uint64
	BytesOut    uint64
}

// getLocalSubnets returns all local network subnets in the workspace
// It collects all local subnets from the network interfaces.
func getLocalSubnets() ([]*net.IPNet, error) {
	var subnets []*net.IPNet

	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("failed to get network interfaces: %v", err)
	}

	for _, iface := range interfaces {
		// Skip interfaces that are down or not loopback
		if iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			_, subnet, err := net.ParseCIDR(addr.String())
			if err == nil {
				subnets = append(subnets, subnet)
			}
		}
	}

	return subnets, nil
}

// ResolveHostnameToIP resolves the hostname to IP address
func resolveHostnameToIP(host string) (net.IP, error) {
	ip := net.ParseIP(host)
	if ip != nil {
		return ip, nil
	}

	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve %s: %w", host, err)
	}
	return ips[0], nil
}

// Get the port-forward ID from the gRPC metadata "portforward-id"
func (s *RemoteOSServiceServer) GetPortForwardID(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Errorf(codes.InvalidArgument, "missing metadata")
	}

	data := md.Get("portforward-id")
	if len(data) == 0 {
		return "", status.Errorf(codes.InvalidArgument, "missing portforward-id")
	}

	return data[0], nil
}

// Get the port-forward data from the store. It uses the port-forward ID retrieved from the metadata
func (s *RemoteOSServiceServer) GetPortForwardData(ctx context.Context) (*PortForwardData, error) {
	pfID, err := s.GetPortForwardID(ctx)
	if err != nil {
		return nil, err
	}
	value, ok := s.grpcApi.portForwardStore.Load(pfID)
	if !ok {
		return nil, status.Errorf(codes.NotFound, "port-forward id %s not found", pfID)
	}
	return value.(*PortForwardData), nil
}

func (s *RemoteOSServiceServer) createTCPConnection(destination string, port uint16) (net.Conn, error) {
	// Resolve the destination hostname to IP
	destinationIP, err := resolveHostnameToIP(destination)
	if err != nil {
		return nil, status.Errorf(
			codes.NotFound,
			"failed to resolve destination IP: %v", err,
		)
	}

	// Check if the destination IP is in the allowed subnets
	// Collect all local subnets if the rule is "localnetworks:0"
	localSubnets := []*net.IPNet{}
	allowRules := make([]PortForwardingRule, len(s.grpcApi.portForwadingRules))
	copy(allowRules, s.grpcApi.portForwadingRules)
	for _, rule := range s.grpcApi.portForwadingRules {
		if rule.Subnet == nil {
			if len(localSubnets) == 0 {
				localSubnets, err = getLocalSubnets()
				if err != nil {
					return nil, status.Errorf(
						codes.Internal,
						"failed to get local subnets: %v", err,
					)
				}
			}
			for _, subnet := range localSubnets {
				allowRules = append(allowRules, PortForwardingRule{Subnet: subnet, Port: rule.Port})
			}
		}
	}

	// Evaluate the rules
	found := false
	for _, rule := range allowRules {
		if rule.Subnet == nil {
			continue
		}
		if rule.Subnet.Contains(destinationIP) && (rule.Port == port || rule.Port == 0) {
			found = true
		}
	}
	if !found {
		return nil, status.Errorf(
			codes.PermissionDenied,
			"destination %s:%d is not in allowed networks", destinationIP, port,
		)
	}

	var tcpConn net.Conn
	tcpConn, err = net.Dial("tcp", net.JoinHostPort(destination, fmt.Sprintf("%d", port)))
	if err != nil {
		return nil, status.Errorf(
			codes.Unavailable,
			"failed to connect to %s:%d: %v", destination, port, err,
		)
	}

	return tcpConn, nil
}

// Run the port-forwarding service. It reads data from the TCP connection and sends it to the gRPC client. It also
// reads data from the gRPC client and sends it to the TCP connection.
func (s *RemoteOSServiceServer) PortForward(stream k8shelldpb.RemoteOSService_PortForwardServer) error {
	pfID, err := s.GetPortForwardID(stream.Context())
	if err != nil {
		return fmt.Errorf("failed to get port-forward ID: %v", err)
	}

	logger := log.NewLogger("grpc-portforward")

	// The first request is the command
	req, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "failed to receive command: %v", err)
	}

	logger.Info().Msgf("Port-forward request: %v", req)

	dstReq, ok := req.Request.(*k8shelldpb.PortForwardRequest_Destination)
	if !ok {
		return status.Errorf(codes.InvalidArgument, "invalid port-forward request: %v", req)
	}

	pf := &PortForwardData{
		Id:          pfID,
		Destination: dstReq.Destination.Ip,
		Port:        uint16(dstReq.Destination.Port),
		Created:     time.Now(),
		Deleted:     time.Time{},
		BytesIn:     0,
		BytesOut:    0,
	}

	tcpConn, err := s.createTCPConnection(pf.Destination, pf.Port)
	if err != nil {
		return status.Errorf(codes.Unavailable, "failed to create TCP connection: %v", err)
	}
	s.grpcApi.portForwardStore.Store(pf.Id, pf)

	logger.Info().Msgf("Port-forward started, id=%s, %s:%d", pf.Id, pf.Destination, pf.Port)

	go func() {
		buf := make([]byte, DEFAULT_MAX_PACKET_SIZE)

		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				if err == io.EOF {
					logger.Info().Msg("TCP connection closed")
				} else {
					logger.Error().Msgf("Error reading from TCP connection: %v", err)
				}
				stream.Send(&k8shelldpb.PortForwardResponse{
					Response: &k8shelldpb.PortForwardResponse_Terminate{Terminate: true}})
				return
			}

			// Send received data to the gRPC client
			err = stream.Send(&k8shelldpb.PortForwardResponse{
				Response: &k8shelldpb.PortForwardResponse_Data{Data: buf[:n]}})
			if err != nil {
				logger.Error().Msgf("Failed to send data to gRPC client: %v", err)
				return
			}
			pf.BytesOut += uint64(n)
		}
	}()

	for {
		// Receive data from gRPC client
		req, err := stream.Recv()
		if err == io.EOF {
			logger.Info().Msg("Client closed the stream")
			break
		}
		if err != nil {
			logger.Error().Msgf("failed to receive: %v", err)
			break
		}

		isError := false

		switch req.Request.(type) {
		case *k8shelldpb.PortForwardRequest_Data:
			data := req.GetData()
			if _, err := tcpConn.Write(data); err != nil {
				logger.Error().Msgf("Failed to write data to TCP connection: %v", err)
				isError = true
				break
			}
			pf.BytesIn += uint64(len(data))
		case *k8shelldpb.PortForwardRequest_Destination:
			logger.Error().Msg("invalid request type, expected Data")
			isError = true
		}

		// Terminate the port-forwarding if there is an error
		if isError {
			break
		}

	}

	tcpConn.Close()
	pf.Deleted = time.Now()

	logger.Info().Msgf("Port-forward stream ended: id=%s, duration=%s, bytes_in=%d, bytes_out=%d",
		pf.Id, time.Since(pf.Created), pf.BytesIn, pf.BytesOut)

	return nil
}
