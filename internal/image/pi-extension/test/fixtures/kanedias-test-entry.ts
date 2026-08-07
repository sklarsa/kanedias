import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";
import kanediasExtension from "../../src/index.ts";

// Test-only entrypoint for Pi process integration. Production loads src/index.ts
// directly; this wrapper only injects a temporary workspace containment root.
export default function kanediasTestEntry(pi: ExtensionAPI): Promise<void> {
  const workspaceRoot = process.env.KANEDIAS_TEST_WORKSPACE_ROOT;
  return kanediasExtension(pi, workspaceRoot ? { workspaceRoot } : {});
}
