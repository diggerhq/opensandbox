/**
 * Runtime module - pluggable sandbox execution backends
 */

export * from "./types";
export { virtualboxRuntime, VirtualBoxRuntime } from "./virtualbox";
export { firecrackerRuntime, FirecrackerRuntime } from "./firecracker";
export { dockerRuntime, DockerRuntime } from "./docker";

import { runtimeRegistry } from "./types";
import { virtualboxRuntime } from "./virtualbox";
import { firecrackerRuntime } from "./firecracker";
import { dockerRuntime } from "./docker";

// Register available runtimes
runtimeRegistry.register(virtualboxRuntime);
runtimeRegistry.register(firecrackerRuntime);
runtimeRegistry.register(dockerRuntime);

// Set default based on environment or availability
const defaultRuntime = process.env.SANDBOX_RUNTIME || "virtualbox";
try {
  runtimeRegistry.setDefault(defaultRuntime);
} catch {
  console.warn(`Runtime "${defaultRuntime}" not available, will auto-detect`);
}

export { runtimeRegistry };

/**
 * Get the active runtime (auto-detects if needed)
 */
export async function getRuntime() {
  try {
    const runtime = runtimeRegistry.get();
    if (await runtime.isAvailable()) {
      return runtime;
    }
  } catch {}

  // Auto-detect
  const available = await runtimeRegistry.findAvailable();
  if (!available) {
    throw new Error("No sandbox runtime available");
  }

  console.log(`[Runtime] Auto-detected: ${available.name}`);
  runtimeRegistry.setDefault(available.name);
  return available;
}

