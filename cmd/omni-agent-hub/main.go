// Omni A2A Gateway - A Go implementation of an A2A protocol server.
//
// This server acts as:
// 1. An A2A server itself (backed by the local Hermes CLI agent)
// 2. An A2A gateway/aggregator that proxies requests to upstream A2A agents
// 3. A central hub where A2A clients connect to one endpoint
//
// Usage:
//
//	omni-agent-hub [serve] [--config config.yaml] [--host 0.0.0.0] [--port 8222]
//	omni-agent-hub logs [-f] [-n 50]
package main

import (
	"fmt"
	"os"

	"github.com/OmniLLM/omni-agent-hub/internal/cli"
)

func main() {
	if err := cli.NewRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}
