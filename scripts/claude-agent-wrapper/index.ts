#!/usr/bin/env node

/**
 * claude-agent-wrapper
 *
 * In-VM wrapper that bridges the Claude Agent SDK with a JSON-line stdio protocol.
 * Runs as an exec session process: reads JSON commands from stdin, emits structured
 * events as JSON lines on stdout.
 *
 * Stdin protocol (JSON lines):
 *   {"type":"configure","model":"...","allowedTools":[...],...}
 *   {"type":"prompt","text":"Fix the bug in auth.py"}
 *   {"type":"interrupt"}
 *
 * Stdout protocol (JSON lines):
 *   {"type":"ready"}
 *   {"type":"assistant","message":{...},"session_id":"..."}
 *   {"type":"result","subtype":"success","result":"...","total_cost_usd":0.01,...}
 *   {"type":"turn_complete"}
 *   {"type":"interrupted"}
 *   {"type":"error","message":"..."}
 */

import { createInterface } from "node:readline";
import { query } from "@anthropic-ai/claude-agent-sdk";
import type { Options, SDKMessage, Query } from "@anthropic-ai/claude-agent-sdk";

// --- Types ---

interface ConfigureCommand {
  type: "configure";
  model?: string;
  allowedTools?: string[];
  disallowedTools?: string[];
  systemPrompt?: string;
  permissionMode?: string;
  maxTurns?: number;
  cwd?: string;
  mcpServers?: Record<string, unknown>;
}

interface PromptCommand {
  type: "prompt";
  text: string;
}

interface InterruptCommand {
  type: "interrupt";
}

type Command = ConfigureCommand | PromptCommand | InterruptCommand;

// --- State ---

let config: ConfigureCommand = {
  type: "configure",
  model: "claude-sonnet-4-20250514",
  permissionMode: "bypassPermissions",
  cwd: process.env.HOME || "/home/user",
};

let activeQuery: Query | null = null;

// --- Helpers ---

function emit(event: Record<string, unknown>): void {
  process.stdout.write(JSON.stringify(event) + "\n");
}

function logError(msg: string): void {
  process.stderr.write(`[claude-agent-wrapper] ${msg}\n`);
}

// --- Command handlers ---

function handleConfigure(cmd: ConfigureCommand): void {
  config = { ...config, ...cmd };
  emit({ type: "configured" });
}

async function handlePrompt(cmd: PromptCommand): Promise<void> {
  const options: Options = {
    maxTurns: config.maxTurns ?? 50,
    permissionMode: (config.permissionMode as Options["permissionMode"]) ?? "bypassPermissions",
    allowDangerouslySkipPermissions: config.permissionMode === "bypassPermissions",
  };

  if (config.model) {
    options.model = config.model;
  }

  if (config.systemPrompt) {
    options.systemPrompt = config.systemPrompt;
  }

  if (config.cwd) {
    options.cwd = config.cwd;
  }

  if (config.allowedTools && config.allowedTools.length > 0) {
    options.allowedTools = config.allowedTools;
  }

  if (config.disallowedTools && config.disallowedTools.length > 0) {
    options.disallowedTools = config.disallowedTools;
  }

  if (config.mcpServers) {
    options.mcpServers = config.mcpServers as Options["mcpServers"];
  }

  try {
    activeQuery = query({ prompt: cmd.text, options });

    for await (const message of activeQuery) {
      emitMessage(message);
    }

    emit({ type: "turn_complete" });
  } catch (err: unknown) {
    const message = err instanceof Error ? err.message : String(err);
    if (message.includes("abort") || message.includes("interrupt")) {
      emit({ type: "interrupted" });
    } else {
      logError(`prompt error: ${message}`);
      emit({ type: "error", message });
    }
  } finally {
    activeQuery = null;
  }
}

function emitMessage(msg: SDKMessage): void {
  // Forward the message as-is — the SDK client will parse by `type` field
  // Strip any non-serializable properties
  const serializable: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(msg)) {
    if (typeof value !== "function") {
      serializable[key] = value;
    }
  }
  emit(serializable);
}

async function handleInterrupt(): Promise<void> {
  if (activeQuery) {
    try {
      await activeQuery.interrupt();
    } catch {
      // Query may already be done
    }
  } else {
    emit({ type: "interrupted" });
  }
}

// --- Main loop ---

async function main(): Promise<void> {
  // Verify API key is available
  if (!process.env.ANTHROPIC_API_KEY) {
    emit({ type: "error", message: "ANTHROPIC_API_KEY environment variable is not set" });
    process.exit(1);
  }

  emit({ type: "ready" });

  const rl = createInterface({ input: process.stdin });

  for await (const line of rl) {
    const trimmed = line.trim();
    if (!trimmed) continue;

    let cmd: Command;
    try {
      cmd = JSON.parse(trimmed) as Command;
    } catch {
      logError(`invalid JSON: ${trimmed}`);
      emit({ type: "error", message: "invalid JSON input" });
      continue;
    }

    switch (cmd.type) {
      case "configure":
        handleConfigure(cmd);
        break;
      case "prompt":
        if (!cmd.text) {
          emit({ type: "error", message: "prompt text is required" });
          break;
        }
        await handlePrompt(cmd);
        break;
      case "interrupt":
        await handleInterrupt();
        break;
      default:
        emit({ type: "error", message: `unknown command type: ${(cmd as { type: string }).type}` });
    }
  }
}

main().catch((err) => {
  logError(`fatal: ${err}`);
  process.exit(1);
});
