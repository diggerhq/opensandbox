/**
 * OpenSandbox client for creating and managing sandbox sessions.
 */

import * as grpc from '@grpc/grpc-js';
import * as protoLoader from '@grpc/proto-loader';
import { Sandbox } from './sandbox';
import { ConnectionError } from './errors';
import type { ClientOptions, CreateOptions } from './types';

// gRPC service definition loaded from proto
interface SandboxServiceClient {
  new (address: string, credentials: grpc.ChannelCredentials): SandboxServiceClient;
}

interface ProtoGrpcType {
  sandbox: {
    SandboxService: SandboxServiceClient;
  };
}

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
export class OpenSandbox {
  private _baseUrl: string;
  private _grpcPort: number;
  private _grpcInsecure: boolean;
  private _timeout: number;
  private _grpcChannel: grpc.Channel | null = null;
  private _grpcStub: any = null;
  private _host: string;
  private _grpcSecure: boolean;

  /**
   * Initialize the OpenSandbox client.
   *
   * @param baseUrl - Base URL of the OpenSandbox server (e.g., "http://localhost:8080")
   * @param options - Client options
   */
  constructor(baseUrl: string, options: ClientOptions = {}) {
    this._baseUrl = baseUrl.replace(/\/$/, '');
    this._grpcPort = options.grpcPort ?? 50051;
    this._grpcInsecure = options.grpcInsecure ?? false;
    this._timeout = options.timeout ?? 30000;

    // Parse URL to get host
    const url = new URL(this._baseUrl);
    this._host = url.hostname;
    // Use secure gRPC for HTTPS unless explicitly set to insecure
    this._grpcSecure = url.protocol === 'https:' && !this._grpcInsecure;
  }

  /**
   * Create a new sandbox session.
   *
   * @param options - Options for creating the sandbox
   * @returns A Sandbox instance ready for use
   * @throws {ConnectionError} If connection to the server fails
   */
  async create(options: CreateOptions = {}): Promise<Sandbox> {
    const { env = {}, timeout = 300 } = options;

    try {
      const controller = new AbortController();
      const timeoutId = setTimeout(() => controller.abort(), this._timeout);

      const response = await fetch(`${this._baseUrl}/sessions`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ env }),
        signal: controller.signal,
      });

      clearTimeout(timeoutId);

      if (!response.ok) {
        throw new ConnectionError(`Failed to create sandbox: ${response.status} ${response.statusText}`);
      }

      // Server returns snake_case, we need to map to camelCase
      const data = await response.json() as {
        session_id: string;
        preview_url: string | null;
      };

      return new Sandbox(
        data.session_id,
        this._grpcStub,
        this._baseUrl,
        data.preview_url
      );
    } catch (error) {
      if (error instanceof ConnectionError) {
        throw error;
      }
      throw new ConnectionError(`Failed to create sandbox: ${error}`);
    }
  }

  /**
   * Initialize gRPC connection (optional, for better performance).
   *
   * Call this method before creating sandboxes if you want to use gRPC
   * for command execution and file operations.
   */
  async connectGrpc(): Promise<void> {
    const grpcTarget = `${this._host}:${this._grpcPort}`;

    const credentials = this._grpcSecure
      ? grpc.credentials.createSsl()
      : grpc.credentials.createInsecure();

    // Load proto file dynamically
    // Note: In a real implementation, you'd either bundle the proto file
    // or generate TypeScript types from it
    try {
      const packageDefinition = protoLoader.loadSync('sandbox.proto', {
        keepCase: true,
        longs: String,
        enums: String,
        defaults: true,
        oneofs: true,
      });
      const proto = grpc.loadPackageDefinition(packageDefinition) as unknown as ProtoGrpcType;
      this._grpcStub = new proto.sandbox.SandboxService(grpcTarget, credentials);
    } catch {
      // gRPC connection is optional - SDK works with HTTP only
      console.warn('gRPC connection not available, using HTTP only');
    }
  }

  /**
   * Close all connections to the server.
   */
  async close(): Promise<void> {
    if (this._grpcChannel) {
      this._grpcChannel.close();
      this._grpcChannel = null;
    }
    this._grpcStub = null;
  }
}
