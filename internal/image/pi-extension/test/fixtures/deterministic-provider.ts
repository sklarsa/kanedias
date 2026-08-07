import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Test-only deterministic OpenAI-compatible provider. It registers no tools or
// commands; test HTTP responses choose which real extension tool Pi invokes.
export default function deterministicProvider(pi: ExtensionAPI): void {
  const baseUrl = process.env.KANEDIAS_TEST_PROVIDER_URL;
  if (!baseUrl) throw new Error("KANEDIAS_TEST_PROVIDER_URL is required");
  pi.registerProvider("kanedias-test", {
    name: "Kanedias Test Provider",
    baseUrl,
    apiKey: "test-key",
    api: "openai-completions",
    models: [{
      id: "deterministic",
      name: "Deterministic",
      reasoning: false,
      input: ["text"],
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
      contextWindow: 32_000,
      maxTokens: 1_024,
    }],
  });
}
