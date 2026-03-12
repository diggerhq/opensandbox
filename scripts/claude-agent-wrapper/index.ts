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
  resume?: string;
}

interface PromptCommand {
  type: "prompt";
  text: string;
  resume?: string;
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
// Track the Claude session ID so we can resume later
let claudeSessionId: string | null = null;

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

  // Resume from a previous Claude session if requested
  // Priority: prompt command resume > config resume > auto-captured session ID
  const resumeId = cmd.resume || config.resume || claudeSessionId;
  if (resumeId) {
    options.resume = resumeId;
    const source = cmd.resume ? "prompt" : config.resume ? "config" : "auto";
    logError(`resuming from session: ${resumeId} (from ${source})`);
    // Clear config.resume after first use so subsequent prompts don't re-resume from config
    if (config.resume) {
      config.resume = undefined;
    }
  }

  logError(`handlePrompt: prompt="${cmd.text.slice(0, 100)}", resume=${resumeId || "none"}, model=${options.model}, cwd=${options.cwd}`);

  try {
    activeQuery = query({ prompt: cmd.text, options });

    for await (const message of activeQuery) {
      // Capture the Claude session ID from messages for future resumption
      if ("session_id" in message && typeof message.session_id === "string") {
        if (!claudeSessionId || claudeSessionId !== message.session_id) {
          claudeSessionId = message.session_id;
          logError(`captured claude session_id: ${claudeSessionId}`);
        }
      }
      emitMessage(message);
    }

    logError(`turn complete, claude_session_id=${claudeSessionId}`);
    emit({ type: "turn_complete", claude_session_id: claudeSessionId });
  } catch (err: unknown) {
    const errMsg = err instanceof Error ? err.message : String(err);
    const errStack = err instanceof Error ? err.stack : "";
    logError(`prompt error: ${errMsg}\n${errStack}`);
    if (errMsg.includes("abort") || errMsg.includes("interrupt")) {
      emit({ type: "interrupted" });
    } else {
      // If resume failed, try again without resume
      if (resumeId) {
        logError(`resume failed, retrying without resume...`);
        options.resume = undefined;
        try {
          activeQuery = query({ prompt: cmd.text, options });
          for await (const message of activeQuery) {
            if ("session_id" in message && typeof message.session_id === "string") {
              if (!claudeSessionId || claudeSessionId !== message.session_id) {
                claudeSessionId = message.session_id;
                logError(`captured claude session_id (retry): ${claudeSessionId}`);
              }
            }
            emitMessage(message);
          }
          logError(`retry turn complete, claude_session_id=${claudeSessionId}`);
          emit({ type: "turn_complete", claude_session_id: claudeSessionId });
        } catch (retryErr: unknown) {
          const retryMsg = retryErr instanceof Error ? retryErr.message : String(retryErr);
          logError(`retry also failed: ${retryMsg}`);
          emit({ type: "error", message: retryMsg });
        }
      } else {
        emit({ type: "error", message: errMsg });
      }
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

process.on("uncaughtException", (err) => {
  logError(`uncaughtException: ${err.message}\n${err.stack}`);
  emit({ type: "error", message: `uncaughtException: ${err.message}` });
});

process.on("unhandledRejection", (reason) => {
  logError(`unhandledRejection: ${reason}`);
  emit({ type: "error", message: `unhandledRejection: ${String(reason)}` });
});

main().catch((err) => {
  logError(`fatal: ${err}`);
  emit({ type: "error", message: `fatal: ${err instanceof Error ? err.message : String(err)}` });
  process.exit(1);
});
