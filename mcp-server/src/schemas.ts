import { z } from 'zod';

// Schema for Keploy Test Case
export const KeployTestCaseSchema = z.object({
    id: z.string(),
    name: z.string(),
    // Generic structure for HTTP/gRPC request/response
    http_req: z.record(z.any()).optional(),
    http_resp: z.record(z.any()).optional(),
    grpc_req: z.record(z.any()).optional(),
    grpc_resp: z.record(z.any()).optional(),
    mocks: z.array(z.string()).optional(), // List of mock IDs
});

export type KeployTestCase = z.infer<typeof KeployTestCaseSchema>;

// Schema for Keploy Mock
export const KeployMockSchema = z.object({
    id: z.string(),
    kind: z.string(), // e.g., 'Mongo', 'Postgres', 'HTTP'
    name: z.string().optional(),
    spec: z.record(z.any()), // The actual mock data
});

export type KeployMock = z.infer<typeof KeployMockSchema>;

// Schema for Keploy Configuration
export const KeployConfigSchema = z.object({
    app: z.object({
        name: z.string(),
        port: z.number().optional(),
    }),
    path: z.string().optional(),
    test: z.record(z.any()).optional(),
    record: z.record(z.any()).optional(),
});

export type KeployConfig = z.infer<typeof KeployConfigSchema>;
