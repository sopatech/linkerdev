package tun

import (
	"log"
	"os/exec"
	"runtime"
)

// setupRouting sets up routing for the TUN interface based on platform
func setupRouting(ifaceName string) error {
	switch runtime.GOOS {
	case "linux":
		return setupRoutingLinux(ifaceName)
	case "darwin":
		return setupRoutingDarwin(ifaceName)
	default:
		log.Printf("Unsupported platform: %s", runtime.GOOS)
		return nil
	}
}

// setupRoutingLinux sets up routing on Linux
func setupRoutingLinux(ifaceName string) error {
	commands := [][]string{
		{"ip", "addr", "add", "10.0.0.1/24", "dev", ifaceName},
		{"ip", "link", "set", ifaceName, "up"},
		{"ip", "route", "add", "10.0.0.0/24", "dev", ifaceName},
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-d", "10.0.0.0/24", "-j", "ACCEPT"},
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", "DNAT", "--to-destination", "10.0.0.1"},
	}

	for _, cmd := range commands {
		if err := exec.Command(cmd[0], cmd[1:]...).Run(); err != nil {
			log.Printf("Failed to run %v: %v", cmd, err)
			return err
		}
	}

	log.Printf("Linux routing setup complete for interface %s", ifaceName)
	return nil
}

// setupRoutingDarwin sets up routing on macOS
func setupRoutingDarwin(ifaceName string) error {
	commands := [][]string{
		{"ifconfig", ifaceName, "10.0.0.1", "10.0.0.1", "up"},
		{"route", "add", "-net", "10.0.0.0/24", "-interface", ifaceName},
	}

	for _, cmd := range commands {
		if err := exec.Command(cmd[0], cmd[1:]...).Run(); err != nil {
			log.Printf("Failed to run %v: %v", cmd, err)
			return err
		}
	}

	log.Printf("macOS routing setup complete for interface %s", ifaceName)
	return nil
}

// cleanupRouting cleans up routing for the TUN interface
func cleanupRouting(ifaceName string) {
	switch runtime.GOOS {
	case "linux":
		cleanupRoutingLinux(ifaceName)
	case "darwin":
		cleanupRoutingDarwin(ifaceName)
	}
}

// cleanupRoutingLinux cleans up routing on Linux
func cleanupRoutingLinux(ifaceName string) {
	commands := [][]string{
		{"iptables", "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-j", "DNAT", "--to-destination", "10.0.0.1"},
		{"iptables", "-t", "nat", "-D", "OUTPUT", "-d", "10.0.0.0/24", "-j", "ACCEPT"},
		{"ip", "route", "del", "10.0.0.0/24", "dev", ifaceName},
		{"ip", "link", "set", ifaceName, "down"},
		{"ip", "addr", "del", "10.0.0.1/24", "dev", ifaceName},
	}

	for _, cmd := range commands {
		exec.Command(cmd[0], cmd[1:]...).Run() // Ignore errors during cleanup
	}

	log.Printf("Linux routing cleanup complete for interface %s", ifaceName)
}

// cleanupRoutingDarwin cleans up routing on macOS
func cleanupRoutingDarwin(ifaceName string) {
	commands := [][]string{
		{"route", "delete", "-net", "10.0.0.0/24", "-interface", ifaceName},
		{"ifconfig", ifaceName, "down"},
	}

	for _, cmd := range commands {
		exec.Command(cmd[0], cmd[1:]...).Run() // Ignore errors during cleanup
	}

	log.Printf("macOS routing cleanup complete for interface %s", ifaceName)
}
