
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { ListToolsRequestSchema, CallToolRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import * as fs from 'fs/promises';
import * as path from 'path';
import * as yaml from 'yaml';

async function getKeployStats(projectPath: string) {
    const keployDir = path.join(projectPath, 'keploy');
    let totalTests = 0;
    let passedTests = 0;
    let failedTests = 0;
    let testSets = 0;
    let lastRun: string | null = null;

    try {
        const entries = await fs.readdir(keployDir, { withFileTypes: true });

        for (const entry of entries) {
            if (entry.isDirectory() && entry.name.startsWith('test-set-')) {
                testSets++;
                const testsDir = path.join(keployDir, entry.name, 'tests');
                try {
                    const testFiles = await fs.readdir(testsDir);
                    totalTests += testFiles.filter(f => f.endsWith('.yaml') || f.endsWith('.yml')).length;
                } catch (e) {
                    console.warn(`Could not read tests dir for ${entry.name}`, e);
                }
            }
        }

        // Read results from the latest test run report
        const reportsDir = path.join(keployDir, 'reports');
        const reportEntries = await fs.readdir(reportsDir, { withFileTypes: true }).catch(() => []);

        // Find the latest test-run-X directory
        const testRuns = reportEntries
            .filter(e => e.isDirectory() && e.name.startsWith('test-run-'))
            .map(e => ({ name: e.name, num: parseInt(e.name.replace('test-run-', '')) }))
            .sort((a, b) => b.num - a.num);

        if (testRuns.length > 0) {
            const latestRun = testRuns[0].name;
            const runDir = path.join(reportsDir, latestRun);
            const reportFiles = await fs.readdir(runDir);

            for (const reportFile of reportFiles) {
                if (reportFile.endsWith('-report.yaml')) {
                    const reportContent = await fs.readFile(path.join(runDir, reportFile), 'utf-8');
                    const report = yaml.parse(reportContent);
                    passedTests += report.success || 0;
                    failedTests += report.failure || 0;
                    if (report.created_at && (!lastRun || report.created_at > parseInt(lastRun))) {
                        lastRun = new Date(report.created_at * 1000).toISOString();
                    }
                }
            }
        }
    } catch (e) {
        throw new Error(`Could not read keploy directory at ${keployDir}. Make sure the path is correct.`);
    }

    return {
        totalTests,
        passedTests,
        failedTests,
        testSets: testSets || 1,
        lastRun: lastRun || new Date().toISOString()
    };
}

export function registerTools(server: Server) {
    // List Tools
    server.setRequestHandler(ListToolsRequestSchema, async () => {
        return {
            tools: [
                {
                    name: "get_test_statistics",
                    description: "Get statistics about test cases (total recorded tests). Provide the absolute path to the project root.",
                    inputSchema: {
                        type: "object",
                        properties: {
                            projectPath: {
                                type: "string",
                                description: "Absolute path to the project root containing the 'keploy' directory."
                            }
                        },
                        required: ["projectPath"]
                    }
                }
            ]
        };
    });

    // Call Tool
    server.setRequestHandler(CallToolRequestSchema, async (request) => {
        if (request.params.name === "get_test_statistics") {
            const projectPath = String(request.params.arguments?.projectPath);
            if (!projectPath) {
                throw new Error("projectPath is required");
            }

            try {
                const stats = await getKeployStats(projectPath);
                return {
                    content: [{
                        type: "text",
                        text: JSON.stringify(stats, null, 2)
                    }]
                };
            } catch (error: any) {
                return {
                    content: [{
                        type: "text",
                        text: `Error: ${error.message}`
                    }],
                    isError: true
                };
            }
        }

        throw new Error(`Tool not found: ${request.params.name}`);
    });
}
