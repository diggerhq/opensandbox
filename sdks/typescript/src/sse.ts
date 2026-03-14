/**
 * Parse an SSE stream from the server.
 *
 * The server emits three event types:
 *   - `build_log`: streamed during image build, contains `{ step, type, message }`
 *   - `error`: build failed, contains `{ error: string }`
 *   - `result`: final result JSON (sandbox data, snapshot info, etc.)
 *
 * @param resp - Fetch Response with content-type text/event-stream
 * @param onLog - Callback for build log messages
 * @returns The parsed result from the "result" event
 */
export async function parseSSEStream<T>(resp: Response, onLog: (msg: string) => void): Promise<T> {
  const reader = resp.body?.getReader();
  if (!reader) throw new Error("No response body for SSE stream");

  const decoder = new TextDecoder();
  let buffer = "";
  let result: T | null = null;

  while (true) {
    const { done, value } = await reader.read();
    if (done) break;

    buffer += decoder.decode(value, { stream: true });
    const parts = buffer.split("\n\n");
    buffer = parts.pop() ?? "";

    for (const part of parts) {
      if (!part.trim()) continue;

      let eventType = "";
      let data = "";
      for (const line of part.split("\n")) {
        if (line.startsWith("event: ")) eventType = line.slice(7);
        else if (line.startsWith("data: ")) data = line.slice(6);
      }

      if (!data) continue;

      switch (eventType) {
        case "build_log": {
          try {
            onLog(JSON.parse(data).message ?? data);
          } catch {
            onLog(data);
          }
          break;
        }
        case "error": {
          let msg = data;
          try { msg = JSON.parse(data).error ?? data; } catch {}
          throw new Error(`Build failed: ${msg}`);
        }
        case "result": {
          result = JSON.parse(data);
          break;
        }
      }
    }
  }

  if (!result) throw new Error("No result received from build stream");
  return result;
}
