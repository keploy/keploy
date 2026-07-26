import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StdioClientTransport } from "@modelcontextprotocol/sdk/client/stdio.js";

async function main() {
    const transport = new StdioClientTransport({
        command: "node",
        args: ["./dist/src/index.js"]
    });

    const client = new Client({
        name: "test-client",
        version: "1.0.0"
    }, {
        capabilities: {}
    });

    await client.connect(transport);

    console.log("Connected to MCP Server");

    try {
        const result = await client.callTool({
            name: "get_test_statistics",
            arguments: {
                projectPath: "/home/xilinx/keploy/mcp-test-project"
            }
        });

        console.log("Tool Result:", JSON.stringify(result, null, 2));
    } catch (error) {
        console.error("Tool Call Failed:", error);
    }

    await client.close();
}

main().catch(console.error);
