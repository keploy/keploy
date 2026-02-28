import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { ListResourcesRequestSchema, ReadResourceRequestSchema, Resource } from "@modelcontextprotocol/sdk/types.js";
import * as fs from 'fs/promises';
import * as path from 'path';

export function registerResources(server: Server) {
    // List Resources
    server.setRequestHandler(ListResourcesRequestSchema, async () => {
        return {
            resources: [
                {
                    uri: "keploy://config",
                    name: "Keploy Configuration",
                    mimeType: "text/yaml"
                },
                {
                    uri: "keploy://tests",
                    name: "Keploy Test Cases",
                    mimeType: "application/json"
                }
            ]
        };
    });

    // Read Resource
    server.setRequestHandler(ReadResourceRequestSchema, async (request) => {
        const uri = request.params.uri;

        if (uri === "keploy://config") {
            const configPath = path.join(process.cwd(), 'keploy.yml');
            let configContent = await fs.readFile(configPath, 'utf-8').catch(() => ""); // Return empty if not found
            if (!configContent) {
                return {
                    contents: []
                }
            }
            return {
                contents: [{
                    uri: uri,
                    text: configContent,
                    mimeType: "text/yaml"
                }]
            };
        }

        if (uri === "keploy://tests") {
            // TODO: Implement listing of actual test files
            return {
                contents: []
            };
        }

        if (uri.startsWith("keploy://tests/")) {
            // TODO: Implement reading of specific test file
            return {
                contents: []
            };
        }

        throw new Error(`Resource not found: ${uri}`);
    });
}
