import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { registerResources } from './handlers/resources.js';
import { registerTools } from './handlers/tools.js';

// Initialize MCP Server
export const server = new Server({
    name: "keploy-mcp-server",
    version: "1.0.0"
}, {
    capabilities: {
        resources: {},
        tools: {}
    }
});

// Register Resources and Tools
registerResources(server);
registerTools(server);

async function main() {
    const transport = new StdioServerTransport();
    await server.connect(transport);
    console.error("Keploy MCP Server running on stdio");
}

// ESM check for main module is different, but for now we'll just run main()
// if this file is imported as side-effect or running directly.
// In ESM 'if (require.main === module)' doesn't exist.
// We can use a simple check or just call main().
// But since index.ts imports this, we should be careful.
// Let's rely on index.ts calling main() or server.connect().
// Actually, to keep it simple, let's export startServer and call it in index.ts
export async function startServer() {
    return main();
}
