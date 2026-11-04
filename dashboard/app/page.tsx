"use client";

import { useEffect, useState } from "react";

type GatewayHealth = { status: string; service: string; time: string };
type Model = { id: string; object?: string; owned_by?: string };

export default function Home() {
  const [health, setHealth] = useState<GatewayHealth | null>(null);
  const [healthError, setHealthError] = useState("");
  const [apiKey, setApiKey] = useState("");
  const [models, setModels] = useState<Model[]>([]);
  const [modelError, setModelError] = useState("");
  const [loadingModels, setLoadingModels] = useState(false);

  useEffect(() => {
    let active = true;
    fetch("/healthz", { cache: "no-store" })
      .then(async (response) => {
        if (!response.ok) throw new Error("Gateway health check failed");
        return (await response.json()) as GatewayHealth;
      })
      .then((data) => { if (active) setHealth(data); })
      .catch((error: unknown) => {
        if (active) setHealthError(error instanceof Error ? error.message : "Gateway is unreachable");
      });
    return () => { active = false; };
  }, []);

  async function loadModels() {
    setLoadingModels(true);
    setModelError("");
    setModels([]);
    try {
      const response = await fetch("/v1/models", {
        headers: { Authorization: "Bearer " + apiKey },
        cache: "no-store",
      });
      const data = await response.json();
      if (!response.ok) throw new Error(data?.error?.message || "Could not load models");
      setModels(Array.isArray(data.data) ? data.data : []);
    } catch (error) {
      setModelError(error instanceof Error ? error.message : "Could not load models");
    } finally {
      setLoadingModels(false);
    }
  }

  return (
    <main className="shell">
      <header className="topbar">
        <a className="brand" href="/"><span className="brand-mark">F</span><span>FANTOM</span></a>
        <span className="topbar-label">AI GATEWAY <span className="dot" /> SELF-HOSTED</span>
      </header>
      <section className="hero">
        <div className="eyebrow"><span className="pulse" /> UNIFIED MODEL ACCESS</div>
        <h1>One gateway.<br /><span>Every model.</span></h1>
        <p className="intro">A lightweight, OpenAI-compatible gateway built for fast streaming, predictable routing and direct control over your provider connections.</p>
        <div className="hero-meta">
          <div><span className="meta-label">ENGINE</span><strong>GO</strong></div>
          <div><span className="meta-label">DASHBOARD</span><strong>NEXT.JS / TS</strong></div>
          <div><span className="meta-label">API</span><strong>OPENAI-COMPATIBLE</strong></div>
        </div>
      </section>
      <section className="models-panel">
        <div className="section-heading">
          <div><span className="section-kicker">01 / PROVIDERS</span><h2>Connected provider lanes</h2></div>
          <span className="count">2 PROVIDERS</span>
        </div>
        <div className="provider-grid">
          <article className="provider-card">
            <span className="provider-mark nvidia">NV</span>
            <div><strong>NVIDIA NIM</strong><span>API key · model catalog · 100s timeout</span></div>
          </article>
          <article className="provider-card">
            <span className="provider-mark opencode">OC</span>
            <div><strong>OpenCode Console</strong><span>Official Console API key · paid / Go models</span></div>
          </article>
        </div>
        <p className="section-copy">OpenCode Free models remain available only through the official OpenCode client. Fantom connects to the supported Console API.</p>
      </section>
      <section className="status-panel">
        <div className="section-heading">
          <div><span className="section-kicker">02 / SYSTEM</span><h2>Gateway status</h2></div>
          <span className={health ? "status-pill online" : healthError ? "status-pill offline" : "status-pill checking"}><span /> {health ? "OPERATIONAL" : healthError ? "UNAVAILABLE" : "CHECKING"}</span>
        </div>
        <div className="status-grid">
          <div className="status-cell"><span className="cell-label">SERVICE</span><strong>{health?.service || "Fantom AI Gateway"}</strong></div>
          <div className="status-cell"><span className="cell-label">HEALTH</span><strong>{health?.status || (healthError ? "Not reachable" : "Checking…")}</strong></div>
          <div className="status-cell"><span className="cell-label">LAST CHECK</span><strong>{health?.time ? new Date(health.time).toLocaleTimeString() : "—"}</strong></div>
        </div>
      </section>
      <section className="models-panel">
        <div className="section-heading">
          <div><span className="section-kicker">03 / MODELS</span><h2>Model catalog</h2></div>
          <span className="count">{models.length.toString().padStart(2, "0")} MODELS</span>
        </div>
        <p className="section-copy">Enter your gateway key to retrieve models from the two configured providers. Your key is used only in this browser session and is not saved.</p>
        <form className="key-form" onSubmit={(event) => { event.preventDefault(); void loadModels(); }}>
          <label htmlFor="gateway-key">GATEWAY API KEY</label>
          <div className="key-row">
            <input id="gateway-key" type="password" autoComplete="off" value={apiKey} onChange={(event) => setApiKey(event.target.value)} placeholder="Paste your gateway key" />
            <button type="submit" disabled={!apiKey.trim() || loadingModels}>{loadingModels ? "LOADING…" : "LOAD MODELS"}</button>
          </div>
        </form>
        {modelError && <p className="error-message">{modelError}</p>}
        {models.length > 0 && <div className="model-list">
          {models.map((model) => <div className="model-row" key={model.id}><span className="model-indicator" /><strong>{model.id}</strong><span>{model.owned_by || "provider"}</span></div>)}
        </div>}
        {models.length === 0 && !modelError && <div className="empty-state">Your available models will appear here.</div>}
      </section>
      <section className="api-panel">
        <div className="section-heading">
          <div><span className="section-kicker">04 / OPTIMIZATION</span><h2>Token saver</h2></div>
          <span className="count">OPT-IN PER REQUEST</span>
        </div>
        <div className="optimizer-row">
          <div><strong>Safe tool-output cleanup</strong><span>ANSI, progress redraws, repeated lines and JSON whitespace</span></div>
          <code>X-Fantom-Token-Saver: safe</code>
        </div>
        <div className="optimizer-row">
          <div><strong>Compact code mode</strong><span>Minimal complete changes without skipping tests or safeguards</span></div>
          <code>X-Fantom-Code-Mode: full</code>
        </div>
        <p className="footnote">User prompts, assistant content and file-read tool results are not rewritten. Estimated token savings are approximate (bytes ÷ 4).</p>
      </section>
      <section className="api-panel">
        <div className="section-heading">
          <div><span className="section-kicker">05 / INTEGRATION</span><h2>Use the API</h2></div>
          <span className="count">CHAT COMPLETIONS</span>
        </div>
        <div className="code-block">
          <div className="code-top"><span>REQUEST</span><span>POST /v1/chat/completions</span></div>
          <pre>{"curl http://localhost:8090/v1/chat/completions \\\n  -H \"Authorization: Bearer YOUR_GATEWAY_KEY\" \\\n  -H \"Content-Type: application/json\" \\\n  -H \"X-Fantom-Token-Saver: safe\" \\\n  -H \"X-Fantom-Code-Mode: full\" \\\n  -d '{\n    \"model\": \"meta/your-model\",\n    \"messages\": [{\"role\":\"user\",\"content\":\"Hello\"}],\n    \"stream\": true\n  }'"} </pre>
        </div>
        <p className="footnote">Base URL: <code>http://localhost:8090/v1</code> · Compatible clients can use this as their OpenAI API endpoint.</p>
      </section>
      <footer className="footer"><span>FANTOM AI GATEWAY</span><span>BUILT WITH GO + TYPESCRIPT</span></footer>
    </main>
  );
}
