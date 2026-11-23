import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";
import * as path from 'path';
import { fileURLToPath } from 'url';

const __filename = fileURLToPath(import.meta.url);
const __dirname = path.dirname(__filename);

async function main() {
    console.log("Starting verification...");

    const transport = new StdioClientTransport({
        command: "npx",
        args: ["tsx", path.join(__dirname, "../src/index.ts")],
    });

    const client = new Client(
        {
            name: "verification-client",
            version: "1.0.0",
        },
        {
            capabilities: {},
        }
    );

    try {
        await client.connect(transport);
        console.log("Connected to MCP Server");

        // List Resources
        console.log("Listing Resources...");
        const resources = await client.listResources();
        console.log("Resources:", JSON.stringify(resources, null, 2));

        if (resources.resources.length === 0) {
            console.error("No resources found!");
        }

        // Read a Resource
        const testResource = resources.resources.find(r => r.uri === "keploy://tests");
        if (testResource) {
            console.log(`Reading resource: ${testResource.uri}`);
            const content = await client.readResource({ uri: testResource.uri });
            console.log("Resource Content:", JSON.stringify(content, null, 2));
        }

        // List Tools
        console.log("Listing Tools...");
        const tools = await client.listTools();
        console.log("Tools:", JSON.stringify(tools, null, 2));

        // Call Tool
        console.log("Calling Tool: get_test_statistics");
        const toolResult = await client.callTool({
            name: "get_test_statistics",
            arguments: { projectPath: "/home/xilinx/keploy/mcp-test-project" }
        });
        console.log("Tool Result:", JSON.stringify(toolResult, null, 2));

        console.log("Verification Complete");
    } catch (error) {
        console.error("Verification failed:", error);
    } finally {
        // client.close(); // SDK might not have close explicitly exposed on Client?
        process.exit(0);
    }
}

main().catch(console.error);
