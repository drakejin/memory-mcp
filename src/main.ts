#!/usr/bin/env node
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { registerTools } from "./server.ts";
import { closeAllStores } from "./store.ts";

const server = new McpServer({ name: "memory-mcp", version: "0.1.0" });
registerTools(server);

function shutdown(): void {
  closeAllStores();
  process.exit(0);
}
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);

const transport = new StdioServerTransport();
await server.connect(transport);
