import { createHash } from "crypto";
import { readFileSync, readdirSync, statSync } from "fs";
import { join, relative } from "path";

export interface ImageStep {
  type: "apt_install" | "pip_install" | "run" | "env" | "workdir" | "add_file" | "add_dir";
  args: Record<string, unknown>;
}

export interface ImageManifest {
  base: string;
  steps: ImageStep[];
}

/**
 * Declarative image builder for OpenSandbox.
 *
 * Defines a reproducible sandbox environment via a fluent API.
 * Under the hood, the manifest is sent to the server which boots a base sandbox,
 * executes each step, checkpoints the result, and caches it by content hash.
 *
 * @example
 * ```typescript
 * const image = Image.base()
 *   .aptInstall(['curl', 'git'])
 *   .pipInstall(['requests', 'pandas'])
 *   .addFile('/workspace/config.json', '{"key": "value"}')
 *   .env({ PROJECT_ROOT: '/workspace' })
 *   .workdir('/workspace')
 *
 * // On-demand: cached by content hash
 * const sandbox = await Sandbox.create({ image })
 *
 * // Pre-built snapshot
 * const snapshots = new Snapshots()
 * await snapshots.create({ name: 'data-science', image })
 * ```
 */
export class Image {
  private readonly manifest: ImageManifest;

  private constructor(steps: ImageStep[] = []) {
    this.manifest = { base: "base", steps };
  }

  /**
   * Create a new image starting from the default OpenSandbox environment
   * (Ubuntu 22.04 with Python, Node.js, build tools, and common utilities).
   * Customize by chaining steps like `.aptInstall()`, `.pipInstall()`, `.runCommands()`, etc.
   */
  static base(): Image {
    return new Image();
  }

  /**
   * Install system packages via apt-get.
   */
  aptInstall(packages: string[]): Image {
    return new Image([
      ...this.manifest.steps,
      { type: "apt_install", args: { packages } },
    ]);
  }

  /**
   * Install Python packages via pip.
   */
  pipInstall(packages: string[]): Image {
    return new Image([
      ...this.manifest.steps,
      { type: "pip_install", args: { packages } },
    ]);
  }

  /**
   * Run one or more shell commands.
   */
  runCommands(...commands: string[]): Image {
    return new Image([
      ...this.manifest.steps,
      { type: "run", args: { commands } },
    ]);
  }

  /**
   * Set environment variables (written to /etc/environment).
   */
  env(vars: Record<string, string>): Image {
    return new Image([
      ...this.manifest.steps,
      { type: "env", args: { vars } },
    ]);
  }

  /**
   * Set the default working directory.
   */
  workdir(path: string): Image {
    return new Image([
      ...this.manifest.steps,
      { type: "workdir", args: { path } },
    ]);
  }

  /**
   * Add a file with inline content to the image.
   * @param remotePath - Absolute path inside the sandbox where the file will be written.
   * @param content - String content of the file.
   */
  addFile(remotePath: string, content: string): Image {
    const encoded = Buffer.from(content).toString("base64");
    return new Image([
      ...this.manifest.steps,
      { type: "add_file", args: { path: remotePath, content: encoded, encoding: "base64" } },
    ]);
  }

  /**
   * Add a local file into the image.
   * Reads the file from disk and embeds its content in the manifest.
   * @param localPath - Path to the file on the local machine.
   * @param remotePath - Absolute path inside the sandbox where the file will be written.
   */
  addLocalFile(localPath: string, remotePath: string): Image {
    const content = readFileSync(localPath);
    const encoded = content.toString("base64");
    return new Image([
      ...this.manifest.steps,
      { type: "add_file", args: { path: remotePath, content: encoded, encoding: "base64" } },
    ]);
  }

  /**
   * Add a local directory into the image.
   * Recursively reads all files and embeds them in the manifest.
   * @param localPath - Path to the directory on the local machine.
   * @param remotePath - Absolute path inside the sandbox where the directory will be created.
   */
  addLocalDir(localPath: string, remotePath: string): Image {
    const files: Array<{ relativePath: string; content: string }> = [];
    collectFiles(localPath, localPath, files);
    return new Image([
      ...this.manifest.steps,
      { type: "add_dir", args: { path: remotePath, files } },
    ]);
  }

  /**
   * Returns the manifest as a plain object (for JSON serialization).
   */
  toJSON(): ImageManifest {
    return this.manifest;
  }

  /**
   * Compute a deterministic content hash for caching.
   */
  cacheKey(): string {
    const canonical = JSON.stringify(this.manifest);
    return createHash("sha256").update(canonical).digest("hex");
  }
}

function collectFiles(
  basePath: string,
  currentPath: string,
  out: Array<{ relativePath: string; content: string }>
): void {
  for (const entry of readdirSync(currentPath)) {
    const full = join(currentPath, entry);
    const stat = statSync(full);
    if (stat.isDirectory()) {
      collectFiles(basePath, full, out);
    } else if (stat.isFile()) {
      out.push({
        relativePath: relative(basePath, full),
        content: readFileSync(full).toString("base64"),
      });
    }
  }
}
