package tun

import (
	"context"
	"fmt"
	"log"
	"net"

	"github.com/songgao/water"
	"k8s.io/client-go/kubernetes"
)

// RelayClient interface for communicating with the relay
type RelayClient interface {
	SendOutboundConnect(destination string) error
}

// StartTransparentProxy starts the TUN interface for transparent proxying
func StartTransparentProxy(ctx context.Context, cs *kubernetes.Clientset, rc RelayClient) {
	// Create TUN interface
	config := water.Config{
		DeviceType: water.TUN,
	}
	iface, err := water.New(config)
	if err != nil {
		log.Printf("Failed to create TUN interface: %v", err)
		return
	}
	defer iface.Close()

	ifaceName := iface.Name()
	log.Printf("Created TUN interface: %s", ifaceName)

	// Setup routing based on platform
	if err := setupRouting(ifaceName); err != nil {
		log.Printf("Failed to setup routing: %v", err)
		return
	}
	defer cleanupRouting(ifaceName)

	// Process packets
	processPackets(ctx, iface, rc)
}

// processPackets processes packets from the TUN interface
func processPackets(ctx context.Context, iface *water.Interface, rc RelayClient) {
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return
		default:
			n, err := iface.Read(buf)
			if err != nil {
				log.Printf("TUN read error: %v", err)
				continue
			}
			handlePacket(buf[:n], rc)
		}
	}
}

// handlePacket handles a packet from the TUN interface
func handlePacket(packet []byte, rc RelayClient) {
	if len(packet) < 20 {
		// Too short to be a valid IP packet
		return
	}

	// Parse IP header
	version := (packet[0] >> 4) & 0x0F
	if version != 4 {
		// Only handle IPv4 for now
		return
	}

	// Extract destination IP
	destIP := net.IP(packet[16:20])

	// Get protocol
	protocol := packet[9]

	// Only handle TCP for now
	if protocol != 6 {
		return
	}

	// Parse TCP header (starts at IP header length)
	ipHeaderLen := (packet[0] & 0x0F) * 4
	if len(packet) < int(ipHeaderLen)+20 {
		// Too short for TCP header
		return
	}

	// Extract destination port
	destPort := int(packet[ipHeaderLen+2])<<8 | int(packet[ipHeaderLen+3])

	// Create destination string
	destination := net.JoinHostPort(destIP.String(), fmt.Sprintf("%d", destPort))

	// Forward via relay client
	if err := rc.SendOutboundConnect(destination); err != nil {
		log.Printf("Failed to send outbound connect for %s: %v", destination, err)
	} else {
		log.Printf("Forwarded connection to %s via relay", destination)
	}
}
