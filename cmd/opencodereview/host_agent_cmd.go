// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/alibaba/open-code-review/internal/hostagentrelay"
	"github.com/spf13/cobra"
)

var hostAgentListen = "127.0.0.1:8373"

var hostAgentCmd = &cobra.Command{
	Use:   "host-agent",
	Short: "Serve explicit model turns to a local MCP host (no account credentials)",
	Long: "A loopback-only, authenticated model-turn mailbox for interactive MCP host agents. " +
		"This does not turn the current ChatGPT session into an API; the MCP client must poll and answer each turn.",
	RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
}

var hostAgentServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start the local model-turn relay used by ocr review --provider host-agent",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return serveHostAgent(cmd.Context(), hostAgentListen)
	},
}

func init() {
	hostAgentServeCmd.Flags().StringVar(&hostAgentListen, "listen", "127.0.0.1:8373", "numeric loopback address for the relay, e.g. 127.0.0.1:8373")
	hostAgentCmd.AddCommand(hostAgentServeCmd)
}

func serveHostAgent(ctx context.Context, listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return fmt.Errorf("host-agent relay must bind only to 127.0.0.1 or ::1, never an externally reachable interface")
	}
	token := strings.TrimSpace(os.Getenv("OCR_HOST_AGENT_TOKEN"))
	relay, err := hostagentrelay.New(token)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("bind host-agent relay: %w", err)
	}
	defer listener.Close()
	server := &http.Server{Handler: relay.Handler(), ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 16 << 10}
	// This output deliberately never contains the bearer secret or prompts.
	fmt.Fprintf(os.Stderr, "[ocr] host-agent relay ready: http://%s/v1/complete (token from OCR_HOST_AGENT_TOKEN)\n", listener.Addr())
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
