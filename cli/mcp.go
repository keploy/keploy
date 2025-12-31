package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/mcp"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func init() {
	Register("mcp", MCP)
}

// MCP creates the 'mcp' command for Model Context Protocol server management
func MCP(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var cmd = &cobra.Command{
		Use:   "mcp",
		Short: "Model Context Protocol (MCP) server for AI agent integration",
		Long: `The MCP command provides tools for AI agents to interact with Keploy.

This enables natural language prompts to trigger mock recording and replay workflows.
AI agents can discover available tools, invoke them, and receive structured responses.

Examples:
  keploy mcp serve          # Start the MCP server
  keploy mcp tools          # List available MCP tools
  keploy mcp invoke <tool>  # Invoke a specific tool`,
	}

	cmd.AddCommand(MCPServe(ctx, logger, cfg, serviceFactory, cmdConfigurator))
	cmd.AddCommand(MCPTools(ctx, logger, cfg))
	cmd.AddCommand(MCPInvoke(ctx, logger, cfg))

	return cmd
}

// MCPServe creates the 'mcp serve' subcommand to start the MCP server
func MCPServe(ctx context.Context, logger *zap.Logger, cfg *config.Config, serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
	var port int
	var transport string

	var cmd = &cobra.Command{
		Use:   "serve",
		Short: "Start the MCP server for AI agent integration",
		Long: `Start the Model Context Protocol server that enables AI agents to interact with Keploy.

The server exposes Keploy's mock recording and replay capabilities as MCP tools
that can be discovered and invoked by AI agents using natural language.

Supported transports:
  - http: HTTP server on specified port (default)
  - stdio: Standard input/output for direct integration`,
		Example: `  keploy mcp serve
  keploy mcp serve --port 8080
  keploy mcp serve --transport stdio`,
		RunE: func(cmd *cobra.Command, args []string) error {
			server := mcp.NewServer(logger, cfg)

			logger.Info("Starting MCP server",
				zap.String("transport", transport),
				zap.Int("port", port),
			)

			if transport == "stdio" {
				return runMCPStdio(ctx, logger, server)
			}

			return runMCPHTTP(ctx, logger, server, port)
		},
	}

	cmd.Flags().IntVar(&port, "port", 8090, "Port for HTTP transport")
	cmd.Flags().StringVar(&transport, "transport", "http", "Transport type (http, stdio)")

	return cmd
}

// MCPTools creates the 'mcp tools' subcommand to list available tools
func MCPTools(ctx context.Context, logger *zap.Logger, cfg *config.Config) *cobra.Command {
	var outputFormat string

	var cmd = &cobra.Command{
		Use:   "tools",
		Short: "List available MCP tools",
		Long:  `List all available MCP tools that can be invoked by AI agents.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			server := mcp.NewServer(logger, cfg)
			tools := server.GetTools()

			switch outputFormat {
			case "json":
				data, err := json.MarshalIndent(tools, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
			default:
				fmt.Println("Available MCP Tools:")
				fmt.Println("====================")
				for _, tool := range tools {
					fmt.Printf("\n📦 %s\n", tool.Name)
					fmt.Printf("   %s\n", tool.Description)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&outputFormat, "output", "text", "Output format (text, json)")

	return cmd
}

// MCPInvoke creates the 'mcp invoke' subcommand to invoke a specific tool
func MCPInvoke(ctx context.Context, logger *zap.Logger, cfg *config.Config) *cobra.Command {
	var paramsJSON string

	var cmd = &cobra.Command{
		Use:   "invoke <tool-name>",
		Short: "Invoke an MCP tool directly",
		Long: `Invoke an MCP tool directly from the command line.

This is useful for testing and debugging MCP tool implementations.`,
		Example: `  keploy mcp invoke keploy_mock_record --params '{"testCommand": "go test ./..."}'
  keploy mcp invoke keploy_list_mocks
  keploy mcp invoke keploy_generate_tests --params '{"testCommand": "npm test", "apiDescription": "User API"}'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			toolName := args[0]
			server := mcp.NewServer(logger, cfg)

			// Parse parameters
			params := make(map[string]interface{})
			if paramsJSON != "" {
				if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
					return fmt.Errorf("invalid JSON parameters: %w", err)
				}
			}

			// Invoke tool
			result, err := server.InvokeTool(ctx, toolName, params)
			if err != nil {
				return fmt.Errorf("tool invocation failed: %w", err)
			}

			// Output result
			if result.IsError {
				fmt.Println("❌ Error:")
			} else {
				fmt.Println("✅ Result:")
			}

			for _, content := range result.Content {
				fmt.Println(content.Text)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&paramsJSON, "params", "", "Tool parameters as JSON")

	return cmd
}

// runMCPHTTP starts the MCP server with HTTP transport
func runMCPHTTP(ctx context.Context, logger *zap.Logger, server *mcp.Server, port int) error {
	mux := http.NewServeMux()

	// MCP protocol endpoints
	mux.HandleFunc("/tools/list", func(w http.ResponseWriter, r *http.Request) {
		tools := server.GetTools()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"tools": tools,
		})
	})

	mux.HandleFunc("/tools/call", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name      string                 `json:"name"`
			Arguments map[string]interface{} `json:"arguments"`
		}

		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		result, err := server.InvokeTool(r.Context(), req.Name, req.Arguments)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "healthy",
			"server": "keploy-mcp",
		})
	})

	addr := fmt.Sprintf(":%d", port)
	httpServer := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	// Handle graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		logger.Info("Shutting down MCP server...")
		httpServer.Shutdown(ctx)
	}()

	logger.Info("MCP server started",
		zap.String("address", addr),
		zap.String("tools_endpoint", fmt.Sprintf("http://localhost%s/tools/list", addr)),
	)

	fmt.Printf("\n🚀 Keploy MCP Server running at http://localhost%s\n", addr)
	fmt.Printf("   Tools endpoint: http://localhost%s/tools/list\n", addr)
	fmt.Printf("   Invoke endpoint: http://localhost%s/tools/call\n\n", addr)

	return httpServer.ListenAndServe()
}

// runMCPStdio runs the MCP server with stdio transport
func runMCPStdio(ctx context.Context, logger *zap.Logger, server *mcp.Server) error {
	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			var request struct {
				JSONRPC string                 `json:"jsonrpc"`
				ID      interface{}            `json:"id"`
				Method  string                 `json:"method"`
				Params  map[string]interface{} `json:"params"`
			}

			if err := decoder.Decode(&request); err != nil {
				utils.LogError(logger, err, "failed to decode request")
				continue
			}

			var result interface{}
			var rpcErr *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}

			switch request.Method {
			case "tools/list":
				result = map[string]interface{}{
					"tools": server.GetTools(),
				}
			case "tools/call":
				name, _ := request.Params["name"].(string)
				args, _ := request.Params["arguments"].(map[string]interface{})
				toolResult, err := server.InvokeTool(ctx, name, args)
				if err != nil {
					rpcErr = &struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					}{
						Code:    -32603,
						Message: err.Error(),
					}
				} else {
					result = toolResult
				}
			default:
				rpcErr = &struct {
					Code    int    `json:"code"`
					Message string `json:"message"`
				}{
					Code:    -32601,
					Message: fmt.Sprintf("method not found: %s", request.Method),
				}
			}

			response := map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      request.ID,
			}

			if rpcErr != nil {
				response["error"] = rpcErr
			} else {
				response["result"] = result
			}

			if err := encoder.Encode(response); err != nil {
				utils.LogError(logger, err, "failed to encode response")
			}
		}
	}
}
