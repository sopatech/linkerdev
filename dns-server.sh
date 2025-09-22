#!/bin/bash

# Simple script to run linkerdev DNS server permanently
# This keeps the DNS server running so cluster services can be resolved locally

echo "Starting linkerdev DNS server..."
echo "This will keep running until you stop it with Ctrl+C"
echo "DNS server will resolve cluster services to localhost"

# Use a dummy service that doesn't exist to avoid conflicts
# The DNS server will still start and serve all cluster services
./linkerdev -svc api.apps -p 9999 sleep 100000
