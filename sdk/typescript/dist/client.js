"use strict";
/**
 * OpenSandbox client for creating and managing sandbox sessions.
 */
var __createBinding = (this && this.__createBinding) || (Object.create ? (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    var desc = Object.getOwnPropertyDescriptor(m, k);
    if (!desc || ("get" in desc ? !m.__esModule : desc.writable || desc.configurable)) {
      desc = { enumerable: true, get: function() { return m[k]; } };
    }
    Object.defineProperty(o, k2, desc);
}) : (function(o, m, k, k2) {
    if (k2 === undefined) k2 = k;
    o[k2] = m[k];
}));
var __setModuleDefault = (this && this.__setModuleDefault) || (Object.create ? (function(o, v) {
    Object.defineProperty(o, "default", { enumerable: true, value: v });
}) : function(o, v) {
    o["default"] = v;
});
var __importStar = (this && this.__importStar) || (function () {
    var ownKeys = function(o) {
        ownKeys = Object.getOwnPropertyNames || function (o) {
            var ar = [];
            for (var k in o) if (Object.prototype.hasOwnProperty.call(o, k)) ar[ar.length] = k;
            return ar;
        };
        return ownKeys(o);
    };
    return function (mod) {
        if (mod && mod.__esModule) return mod;
        var result = {};
        if (mod != null) for (var k = ownKeys(mod), i = 0; i < k.length; i++) if (k[i] !== "default") __createBinding(result, mod, k[i]);
        __setModuleDefault(result, mod);
        return result;
    };
})();
Object.defineProperty(exports, "__esModule", { value: true });
exports.OpenSandbox = void 0;
const grpc = __importStar(require("@grpc/grpc-js"));
const protoLoader = __importStar(require("@grpc/proto-loader"));
const sandbox_1 = require("./sandbox");
const errors_1 = require("./errors");
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
class OpenSandbox {
    _baseUrl;
    _grpcPort;
    _grpcInsecure;
    _timeout;
    _grpcChannel = null;
    _grpcStub = null;
    _host;
    _grpcSecure;
    /**
     * Initialize the OpenSandbox client.
     *
     * @param baseUrl - Base URL of the OpenSandbox server (e.g., "http://localhost:8080")
     * @param options - Client options
     */
    constructor(baseUrl, options = {}) {
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
    async create(options = {}) {
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
                throw new errors_1.ConnectionError(`Failed to create sandbox: ${response.status} ${response.statusText}`);
            }
            // Server returns snake_case, we need to map to camelCase
            const data = await response.json();
            return new sandbox_1.Sandbox(data.session_id, this._grpcStub, this._baseUrl, data.preview_url);
        }
        catch (error) {
            if (error instanceof errors_1.ConnectionError) {
                throw error;
            }
            throw new errors_1.ConnectionError(`Failed to create sandbox: ${error}`);
        }
    }
    /**
     * Initialize gRPC connection (optional, for better performance).
     *
     * Call this method before creating sandboxes if you want to use gRPC
     * for command execution and file operations.
     */
    async connectGrpc() {
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
            const proto = grpc.loadPackageDefinition(packageDefinition);
            this._grpcStub = new proto.sandbox.SandboxService(grpcTarget, credentials);
        }
        catch {
            // gRPC connection is optional - SDK works with HTTP only
            console.warn('gRPC connection not available, using HTTP only');
        }
    }
    /**
     * Close all connections to the server.
     */
    async close() {
        if (this._grpcChannel) {
            this._grpcChannel.close();
            this._grpcChannel = null;
        }
        this._grpcStub = null;
    }
}
exports.OpenSandbox = OpenSandbox;
//# sourceMappingURL=client.js.map