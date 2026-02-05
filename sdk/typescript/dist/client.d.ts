/**
 * OpenSandbox client for creating and managing sandbox sessions.
 */
import { Sandbox } from './sandbox';
import type { ClientOptions, CreateOptions } from './types';
/**
 * Client for connecting to an OpenSandbox server.
 *
 * This client uses HTTP for sandbox lifecycle (create/destroy) and
 * optionally gRPC for fast command execution and file operations.
 *
 * @example
 * ```typescript
 * const client = new OpenSandbox('http://localhost:8080');
 * const sandbox = await client.create();
 * const result = await sandbox.run('echo hello');
 * console.log(result.stdout); // "hello\n"
 * await sandbox.destroy();
 * await client.close();
 * ```
 */
export declare class OpenSandbox {
    private _baseUrl;
    private _grpcPort;
    private _grpcInsecure;
    private _timeout;
    private _grpcChannel;
    private _grpcStub;
    private _host;
    private _grpcSecure;
    /**
     * Initialize the OpenSandbox client.
     *
     * @param baseUrl - Base URL of the OpenSandbox server (e.g., "http://localhost:8080")
     * @param options - Client options
     */
    constructor(baseUrl: string, options?: ClientOptions);
    /**
     * Create a new sandbox session.
     *
     * @param options - Options for creating the sandbox
     * @returns A Sandbox instance ready for use
     * @throws {ConnectionError} If connection to the server fails
     */
    create(options?: CreateOptions): Promise<Sandbox>;
    /**
     * Initialize gRPC connection (optional, for better performance).
     *
     * Call this method before creating sandboxes if you want to use gRPC
     * for command execution and file operations.
     */
    connectGrpc(): Promise<void>;
    /**
     * Close all connections to the server.
     */
    close(): Promise<void>;
}
//# sourceMappingURL=client.d.ts.map