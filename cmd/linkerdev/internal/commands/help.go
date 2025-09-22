package commands

import "fmt"

// ShowHelp displays the help message
func ShowHelp() {
	fmt.Println("linkerdev - Telepresence-lite via in-cluster relay")
	fmt.Println()
	fmt.Println("USAGE:")
	fmt.Println("  linkerdev -svc <service.namespace> -p <port> <command> [args...]")
	fmt.Println()
	fmt.Println("COMMANDS:")
	fmt.Println("  install         Install relay component in cluster")
	fmt.Println("  uninstall       Remove relay component from cluster")
	fmt.Println("  clean           Clean up leftover resources")
	fmt.Println("  version         Show version information")
	fmt.Println("  help            Show this help message")
	fmt.Println()
	fmt.Println("EXAMPLES:")
	fmt.Println("  # Install the relay component")
	fmt.Println("  sudo linkerdev install")
	fmt.Println()
	fmt.Println("  # Run your service with linkerdev")
	fmt.Println("  linkerdev -svc api-service.apps -p 8080 go run main.go")
	fmt.Println("  linkerdev -svc web-service.apps -p 3000 npm start")
	fmt.Println()
	fmt.Println("  # Clean up resources")
	fmt.Println("  linkerdev clean")
}
