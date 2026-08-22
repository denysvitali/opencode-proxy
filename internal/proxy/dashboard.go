package proxy

import (
	"html/template"
	"net/http"
	"strings"
)

type dashboardRow struct {
	Label string
	Value string
}

type dashboardModel struct {
	ID        string
	Free      bool
	Anthropic bool
}

type dashboardPage struct {
	Styles       template.CSS
	CatalogOK    bool
	CatalogError string
	Models       []dashboardModel
	ModelCount   int
	ProxyRows    []dashboardRow
	ProxyHost    string
}

func (s *Server) dashboard(w http.ResponseWriter, request *http.Request) {
	setDashboardHeaders(w)
	page := dashboardPage{
		Styles:    template.CSS(uiStyles),
		ProxyRows: s.proxyRows(),
		ProxyHost: request.Host,
	}
	ids := s.knownModels()
	if ids == nil {
		page.CatalogError = "The OpenCode Zen model catalog is unavailable right now. Requests still pass through with the requested model name unchanged."
	} else {
		page.CatalogOK = true
		for _, id := range ids {
			page.Models = append(page.Models, dashboardModel{
				ID:        id,
				Free:      strings.Contains(id, "-free"),
				Anthropic: strings.HasPrefix(id, "claude-"),
			})
		}
		page.ModelCount = len(page.Models)
	}
	s.renderDashboard(w, page)
}

func (s *Server) renderDashboard(w http.ResponseWriter, page dashboardPage) {
	if err := dashboardTemplate.Execute(w, page); err != nil {
		s.log.WithError(err).Error("render dashboard")
	}
}

func (s *Server) proxyRows() []dashboardRow {
	clientAuth := "Disabled"
	if s.config.Server.APIKey != "" {
		clientAuth = "Enabled"
	}
	upstreamKey := "Not configured"
	if _, source, err := s.config.ResolveAPIKey(); err == nil {
		upstreamKey = "Configured (" + source + ")"
	}
	return []dashboardRow{
		{Label: "Status", Value: "Online"},
		{Label: "Listen address", Value: valueOrDash(s.config.Server.Listen)},
		{Label: "Default model", Value: valueOrDash(s.config.Proxy.DefaultModel)},
		{Label: "Upstream", Value: valueOrDash(s.config.BaseURL)},
		{Label: "Upstream key", Value: upstreamKey},
		{Label: "Client authentication", Value: clientAuth},
	}
}

func valueOrDash(value string) string {
	if value == "" {
		return "—"
	}
	return value
}

var dashboardTemplate = template.Must(template.New("dashboard").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <title>OpenCode Proxy</title>
  <style>{{.Styles}}</style>
</head>
<body>
<main>
  <div class="shell">
    <header class="topbar">
      <div class="brand">
        <div class="brand-mark">
          <div class="logo">o</div>
          <div class="eyebrow">Proxy online</div>
        </div>
        <h1>OpenCode Proxy</h1>
        <p class="lede">Use OpenCode Zen models from Claude Code and other Anthropic-compatible clients.</p>
      </div>
      <nav class="actions" aria-label="Dashboard actions">
        <a class="btn btn-ghost" href="/">Refresh</a>
        <a class="btn btn-ghost" href="/healthz">Health</a>
      </nav>
    </header>

    {{if .CatalogError}}
    <section class="card card-wide">
      <div class="notice notice-error">{{.CatalogError}}</div>
    </section>
    {{end}}

    <section class="card card-wide">
      <div class="card-header">
        <div class="section-label"><span class="dot"></span>Models</div>
      </div>
      {{if .CatalogOK}}
      <p class="subline">Every model available through this proxy. Click a model ID to copy it, then set it with <code>--model</code> or in your client configuration.</p>
      <div class="model-toolbar">
        <input id="model-search" class="model-search" type="search" placeholder="Filter models…" autocomplete="off" spellcheck="false" aria-label="Filter models">
        <span id="model-count" class="model-count">{{.ModelCount}} models</span>
      </div>
      <div id="model-list" class="model-list">
        {{range .Models}}
        <div class="model-row" data-model-id="{{.ID}}">
          <span class="model-id">{{.ID}}{{if .Free}} · free{{end}}{{if .Anthropic}} · claude-family{{end}}</span>
          <button type="button" data-copy="{{.ID}}" aria-label="Copy model ID {{.ID}}">Copy ID</button>
        </div>
        {{end}}
      </div>
      {{else}}
      <p class="subline" style="margin-bottom:0">No models loaded.</p>
      {{end}}
    </section>

    <section class="card card-wide setup">
      <div class="card-header">
        <div class="section-label"><span class="dot"></span>Claude Code</div>
      </div>
      <p class="lede">Point Claude Code at this proxy, then copy and run the command. Include a port if the hostname needs one.</p>
      <div class="setup-grid">
        <div class="setup-block">
          <label for="proxy-host">Proxy hostname</label>
          <input id="proxy-host" type="text" value="{{.ProxyHost}}" autocomplete="off" spellcheck="false">
        </div>
        <div class="setup-block">
          <label for="claude-command">Launch command</label>
          <textarea id="claude-command" readonly aria-label="Claude Code configuration command"></textarea>
          <div class="setup-actions">
            <button id="copy-claude-command" type="button">Copy command</button>
            <span id="copy-status" class="copy-status" aria-live="polite"></span>
          </div>
        </div>
      </div>
      <p class="subline" style="margin-bottom:0">If client authentication is enabled, replace <code>local</code> with the configured API key. Pick any model above with <code>claude --model &lt;model-id&gt;</code>.</p>
    </section>

    <section class="card card-wide setup">
      <div class="card-header">
        <div class="section-label"><span class="dot"></span>Codex CLI</div>
      </div>
      <p class="lede">Codex speaks the OpenAI Responses API; this proxy translates it for you. Add the provider to <code>~/.codex/config.toml</code>.</p>
      <div class="setup-grid">
        <div class="setup-block">
          <label for="codex-provider">Provider snippet (config.toml)</label>
          <textarea id="codex-provider" readonly aria-label="Codex provider configuration"></textarea>
        </div>
        <div class="setup-block">
          <label for="codex-run">Launch command</label>
          <textarea id="codex-command" readonly aria-label="Codex launch command"></textarea>
          <div class="setup-actions">
            <button id="copy-codex-config" type="button">Copy config</button>
            <button id="copy-codex-command" type="button">Copy command</button>
            <span id="codex-copy-status" class="copy-status" aria-live="polite"></span>
          </div>
        </div>
      </div>
      <p class="subline" style="margin-bottom:0">Run once: <code>codex exec -c model_provider=opencode-proxy -m &lt;model-id&gt; "…"</code>, or set <code>model_provider</code> and <code>model</code> at the top of your config.toml. If client authentication is enabled, put the key in <code>OPENAI_API_KEY</code>.</p>
    </section>

    <footer class="footer">Requests are forwarded to OpenCode Zen. Model IDs come straight from the Zen catalog.</footer>
  </div>
</main>
<script>
  const proxyHost = document.getElementById("proxy-host");
  const claudeCommand = document.getElementById("claude-command");
  const copyClaudeCommand = document.getElementById("copy-claude-command");
  const copyStatus = document.getElementById("copy-status");
  function updateClaudeCommand() {
    claudeCommand.value = "ANTHROPIC_BASE_URL=" + window.location.protocol + "//" + proxyHost.value.trim() + " \\\nANTHROPIC_AUTH_TOKEN=local \\\nclaude";
  }
  const codexProvider = document.getElementById("codex-provider");
  const codexCommand = document.getElementById("codex-command");
  function updateCodexConfig() {
    const base = window.location.protocol + "//" + proxyHost.value.trim();
    codexProvider.value = '[model_providers.opencode-proxy]\nname = "opencode-proxy"\nbase_url = "' + base + '/v1"\nenv_key = "OPENAI_API_KEY"\nwire_api = "responses"';
    codexCommand.value = "OPENAI_API_KEY=local codex exec \\\n  -c model_provider=opencode-proxy \\\n  -m <model-id> \\\n  \"your prompt\"";
  }
  proxyHost.addEventListener("input", () => { updateClaudeCommand(); updateCodexConfig(); });
  async function flashStatus(element) {
    element.textContent = "Copied";
    setTimeout(() => { element.textContent = ""; }, 1200);
  }
  copyClaudeCommand.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(claudeCommand.value);
      flashStatus(copyStatus);
    } catch {
      claudeCommand.select();
      document.execCommand("copy");
      flashStatus(copyStatus);
    }
  });
  document.getElementById("copy-codex-config").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(codexProvider.value);
      flashStatus(document.getElementById("codex-copy-status"));
    } catch {
      codexProvider.select();
      document.execCommand("copy");
      flashStatus(document.getElementById("codex-copy-status"));
    }
  });
  document.getElementById("copy-codex-command").addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(codexCommand.value);
      flashStatus(document.getElementById("codex-copy-status"));
    } catch {
      codexCommand.select();
      document.execCommand("copy");
      flashStatus(document.getElementById("codex-copy-status"));
    }
  });
  async function copyText(text) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      const helper = document.createElement("textarea");
      helper.value = text;
      document.body.appendChild(helper);
      helper.select();
      document.execCommand("copy");
      helper.remove();
      return true;
    }
  }
  document.getElementById("model-list").addEventListener("click", async (event) => {
    const button = event.target.closest("button[data-copy]");
    if (!button) return;
    await copyText(button.dataset.copy);
    const original = button.textContent;
    button.textContent = "Copied";
    setTimeout(() => { button.textContent = original; }, 1200);
  });
  const search = document.getElementById("model-search");
  const rows = Array.from(document.querySelectorAll("#model-list .model-row"));
  search.addEventListener("input", () => {
    const query = search.value.trim().toLowerCase();
    let visible = 0;
    for (const row of rows) {
      const match = row.dataset.modelId.toLowerCase().includes(query);
      row.style.display = match ? "" : "none";
      if (match) visible++;
    }
    document.getElementById("model-count").textContent = visible + (visible === 1 ? " model" : " models");
  });
  updateClaudeCommand();
  updateCodexConfig();
</script>
</body>
</html>`))
